package cache

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

var (
	subscriberCacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_subscriber_cache_hits_total",
		Help: "Authentication lookups served from the in-process cache",
	})
	subscriberCacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_subscriber_cache_misses_total",
		Help: "Authentication lookups that fell through to PostgreSQL",
	})
)

// DefaultSubscriberTTL bounds how long a cached authentication record is served.
//
// This is the window in which a subscriber suspended in the database could still
// re-authenticate. It is kept short deliberately, and callers that change
// auth-relevant state should call InvalidateSubscriber rather than rely on it.
// Suspension also issues a Disconnect-Request, so the TTL is a backstop for
// re-authentication, not the primary enforcement path.
const DefaultSubscriberTTL = 60 * time.Second

// SubscriberCacheKey returns the cache key holding a cached auth record.
func SubscriberCacheKey(username string) string {
	return "subscriber:auth:" + username
}

// cachedSubscriber is the stored form. A separate type from radius.Subscriber so
// the cached shape is explicit and does not silently change when a field is
// added to the domain struct.
type cachedSubscriber struct {
	ID           int
	Username     string
	PasswordHash string
	Status       string
	RateLimitStr string
	FUPActive    bool
	FUPThrottle  string
	PlanID       int
	// NotFound records a negative result, so a flood of requests for a username
	// that does not exist cannot turn into a flood of database queries.
	NotFound bool
}

// SubscriberCache is a read-through, in-process cache in front of the
// authentication lookup, implementing SAD's "RADIUS never touches PostgreSQL
// on the hot path" and the ≤5ms budget FR-AAA-002 sets.
//
// It satisfies radius.DBQuerier, so the daemon is unaware it is cached.
//
// This is a single-process cache: on the native single-machine deployment
// radiusd runs as exactly one OS process, so there are no other replicas
// that would need to see an entry this one populates. (The stack ran on
// Redis when radiusd could be scaled to multiple instances behind a shared
// NAS; a single-machine install retires that requirement — see
// internal/localcache's package doc.)
type SubscriberCache struct {
	db    radius.DBQuerier
	store *localcache.Store[cachedSubscriber]
	ttl   time.Duration
}

var _ radius.DBQuerier = (*SubscriberCache)(nil)

// NewSubscriberCache wraps db with an in-process read-through cache.
func NewSubscriberCache(db radius.DBQuerier, ttl time.Duration) *SubscriberCache {
	if ttl <= 0 {
		ttl = DefaultSubscriberTTL
	}
	return &SubscriberCache{db: db, store: localcache.New[cachedSubscriber](0), ttl: ttl}
}

// GetSubscriberByUsername serves from the cache when possible, falling back
// to PostgreSQL and populating the cache on a miss.
func (c *SubscriberCache) GetSubscriberByUsername(ctx context.Context, username string) (*radius.Subscriber, error) {
	key := SubscriberCacheKey(username)

	if entry, ok := c.store.Get(key); ok {
		subscriberCacheHits.Inc()
		if entry.NotFound {
			return nil, nil
		}
		return &radius.Subscriber{
			ID:           entry.ID,
			Username:     entry.Username,
			PasswordHash: entry.PasswordHash,
			Status:       entry.Status,
			RateLimitStr: entry.RateLimitStr,
			FUPActive:    entry.FUPActive,
			FUPThrottle:  entry.FUPThrottle,
			PlanID:       entry.PlanID,
		}, nil
	}
	subscriberCacheMisses.Inc()

	sub, dbErr := c.db.GetSubscriberByUsername(ctx, username)
	if dbErr != nil {
		return nil, dbErr
	}

	c.cacheResult(key, sub, username)
	return sub, nil
}

// cacheResult writes the lookup result, including a negative entry for an
// unknown username.
func (c *SubscriberCache) cacheResult(key string, sub *radius.Subscriber, username string) {
	entry := cachedSubscriber{Username: username, NotFound: true}
	if sub != nil {
		entry = cachedSubscriber{
			ID:           sub.ID,
			Username:     sub.Username,
			PasswordHash: sub.PasswordHash,
			Status:       sub.Status,
			RateLimitStr: sub.RateLimitStr,
			FUPActive:    sub.FUPActive,
			FUPThrottle:  sub.FUPThrottle,
			PlanID:       sub.PlanID,
		}
	}

	ttl := c.ttl
	if entry.NotFound {
		// Negative entries expire faster: a newly provisioned subscriber should
		// not be locked out for a full TTL by a lookup that preceded them.
		ttl = c.ttl / 4
		if ttl < time.Second {
			ttl = time.Second
		}
	}

	c.store.Set(key, entry, ttl)
}

// InvalidateSubscriber drops a cached record so the next authentication reloads
// it. Call this whenever status, plan or FUP state changes: without it, a
// suspension takes up to one TTL to reach the authentication path.
func (c *SubscriberCache) InvalidateSubscriber(_ context.Context, username string) error {
	c.store.Delete(SubscriberCacheKey(username))
	return nil
}
