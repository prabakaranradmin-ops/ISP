package radius

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"layeh.com/radius"
)

// Connection limits — how many simultaneous logins one account may hold.
//
// Nothing limited it before: one username and password could run as many
// PPPoE lines as there were routers to put them in. Every plan now includes
// one connection, and each unit of a catalogue product marked "adds a
// connection" (migration 077) allows one more, billed monthly by the
// ordinary renewal. Subscriber.ConnectionLimit is the sum.
//
// Only logins by password (PAP, EAP) count. A subscriber's registered
// hotspot devices (MAC Auth Bypass) do not, and are not limited.
//
// A login from the device that already holds one of the subscriber's open
// sessions is not a new connection: it is that router redialling a session
// that dropped without its Accounting-Stop reaching us. Counting it would
// lock a paying subscriber out of their own line until the stale row
// cleared.
//
// The limit ships in report mode: nobody is refused, and the console lists
// who is over, so extras can be sold first. Enforcement is
// CONNECTION_LIMIT_MODE=enforce.

// Connection limit modes.
const (
	// ConnectionLimitReport refuses nobody; the console reports who is over.
	ConnectionLimitReport = "report"
	// ConnectionLimitEnforce refuses a login that would put a subscriber
	// over their limit.
	ConnectionLimitEnforce = "enforce"
)

// ConnectionCounter answers the connection-limit questions the
// authentication path asks. Satisfied by *db.FUPStore.
type ConnectionCounter interface {
	// OpenConnections counts the subscriber's open sessions authenticated
	// by password, leaving out any from exceptDevice ("" leaves out none).
	OpenConnections(ctx context.Context, subscriberID int, exceptDevice string) (int, error)
	// StaticAddressHeldElsewhere reports whether an open session of the
	// subscriber's from another device already holds address.
	StaticAddressHeldElsewhere(ctx context.Context, subscriberID int, address, exceptDevice string) (bool, error)
}

// SetConnectionLimit sets the mode and the counter the limit is checked
// against. Without a counter nothing is refused, whatever the mode.
func (d *RadiusDaemon) SetConnectionLimit(mode string, counter ConnectionCounter) {
	d.connLimitMode, d.connCounter = mode, counter
}

var (
	connectionLimitRefused = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_connection_limit_refused_total",
		Help: "Logins refused because the subscriber already held as many connections as they pay for",
	})
	// A failed check lets the login through, so a rising count is the limit
	// not being enforced.
	connectionLimitCheckFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_connection_limit_check_failed_total",
		Help: "Connection-limit checks that could not be made; the login was let through",
	})
)

// overConnectionLimit reports whether this login has to be refused for
// taking the subscriber over their connection limit.
//
// A failed count lets the login through. The limit is a commercial rule,
// not a security control, and refusing paying subscribers because the
// database stumbled would be the worse failure; the metric makes it visible.
func (d *RadiusDaemon) overConnectionLimit(ctx context.Context, sub *Subscriber, r *radius.Request) bool {
	if d.connLimitMode != ConnectionLimitEnforce || d.connCounter == nil {
		return false
	}
	limit := sub.ConnectionLimit
	if limit < 1 {
		limit = 1
	}
	open, err := d.connCounter.OpenConnections(ctx, sub.ID, requestDeviceMAC(r))
	if err != nil {
		connectionLimitCheckFailed.Inc()
		log.Warn().Err(err).Str("username", sub.Username).
			Msg("radius: connection limit could not be checked; login let through")
		return false
	}
	if open < limit {
		return false
	}
	connectionLimitRefused.Inc()
	log.Info().Str("username", sub.Username).Int("open", open).Int("limit", limit).
		Msg("radius: login refused at the subscriber's connection limit")
	return true
}

// withholdStaticAddress reports whether this login must go without the
// subscriber's static address because another of their connections, from
// another device, already holds it. Two sessions handed one address is a
// collision on the NAS, and a subscriber with extra connections would
// otherwise produce one on their second login. The second connection gets
// a dynamic address instead.
//
// A failed check keeps the address, and so does a request with no device to
// compare against: the subscriber is far more likely to be reconnecting the
// line that owns it than opening a second one, and taking a business's
// static address away on a guess is the worse mistake.
func (d *RadiusDaemon) withholdStaticAddress(ctx context.Context, sub *Subscriber, r *radius.Request) bool {
	device := requestDeviceMAC(r)
	if sub.StaticIP == "" || d.connCounter == nil || device == "" {
		return false
	}
	held, err := d.connCounter.StaticAddressHeldElsewhere(ctx, sub.ID, sub.StaticIP, device)
	if err != nil {
		log.Warn().Err(err).Str("username", sub.Username).
			Msg("radius: could not check who holds the static address; sending it")
		return false
	}
	return held
}

// OverLimitSubscriber is one subscriber holding more password-authenticated
// connections than they pay for, as the console's report shows them.
type OverLimitSubscriber struct {
	SubscriberID int
	Username     string
	// Limit is 1 plus their extra-connection products.
	Limit    int
	Sessions []OpenConnection
}

// Over is how many connections they hold beyond their limit.
func (o OverLimitSubscriber) Over() int { return len(o.Sessions) - o.Limit }

// OpenConnection is one open password-authenticated session.
type OpenConnection struct {
	SessionID  string
	NasIP      string
	AssignedIP string
	DeviceMAC  string
	StartedAt  time.Time
}
