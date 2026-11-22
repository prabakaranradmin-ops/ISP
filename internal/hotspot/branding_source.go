package hotspot

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// BrandingStore reads the stored branding. Satisfied by *db.HotspotStore.
//
// PortalBranding returns the zero Branding, not an error, when nothing has
// been saved: an unconfigured deployment is the normal case, not a failure.
type BrandingStore interface {
	PortalBranding(ctx context.Context) (Branding, error)
	PortalLogo(ctx context.Context) (data []byte, contentType, sha256 string, err error)
}

// BrandingCacheTTL bounds how long a saved change can take to reach the page
// on an instance that did not make the save. The instance that did is
// invalidated directly and shows it on the next request.
const BrandingCacheTTL = 30 * time.Second

// BrandingCache sits between the portal and the store.
//
// Cached because the captive portal is the most-requested page in the system
// and the least deserving of a database round trip: every phone that joins
// the network probes it, often several times, before a person ever looks at
// it. Branding changes a few times a year.
//
// Two rules the portal depends on, both in Get:
//
//   - A read failure never breaks the page. The visitor is somebody trying to
//     get online, and refusing them a sign-in form because a logo setting
//     could not be read would be the worst possible trade. They get the last
//     branding that was read successfully, or the defaults.
//   - A failure is not cached as success. The next request tries again rather
//     than the defaults being pinned for a full TTL.
type BrandingCache struct {
	store BrandingStore
	ttl   time.Duration
	now   func() time.Time

	mu       sync.Mutex
	branding Branding
	logo     []byte
	logoType string
	logoHash string
	loaded   time.Time
	have     bool
}

// NewBrandingCache wraps store. A nil store serves the defaults forever, so a
// deployment that wires no store still gets a working page.
func NewBrandingCache(store BrandingStore) *BrandingCache {
	return &BrandingCache{store: store, ttl: BrandingCacheTTL, now: time.Now}
}

// Invalidate drops the cached copy, so the next request reads the store. The
// console calls it after a save so the operator sees their change at once.
func (c *BrandingCache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.have = false
	c.mu.Unlock()
}

// Get returns the branding to render with.
func (c *BrandingCache) Get(ctx context.Context) Branding {
	if c == nil || c.store == nil {
		return Branding{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && c.now().Sub(c.loaded) < c.ttl {
		return c.branding
	}
	if err := c.refreshLocked(ctx); err != nil {
		log.Warn().Err(err).Msg("hotspot: could not read portal branding; serving the last good copy or the defaults")
		return c.branding // zero value — the defaults — if never loaded
	}
	return c.branding
}

// Logo returns the cached logo, or nil when there is none.
func (c *BrandingCache) Logo(ctx context.Context) (data []byte, contentType, sha string) {
	if c == nil || c.store == nil {
		return nil, "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.have || c.now().Sub(c.loaded) >= c.ttl {
		if err := c.refreshLocked(ctx); err != nil {
			log.Warn().Err(err).Msg("hotspot: could not read the portal logo")
		}
	}
	return c.logo, c.logoType, c.logoHash
}

// refreshLocked reads both halves. The logo is only fetched when the stored
// hash differs from the cached one, so the common refresh — nothing changed —
// costs one small row rather than up to 100 KB.
func (c *BrandingCache) refreshLocked(ctx context.Context) error {
	b, err := c.store.PortalBranding(ctx)
	if err != nil {
		return err
	}
	switch {
	case !b.HasLogo:
		c.logo, c.logoType, c.logoHash = nil, "", ""
	case b.LogoSHA256 != c.logoHash:
		data, ct, sha, err := c.store.PortalLogo(ctx)
		if err != nil {
			return err
		}
		c.logo, c.logoType, c.logoHash = data, ct, sha
	}
	c.branding = b
	c.loaded = c.now()
	c.have = true
	return nil
}
