package alerting

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

var (
	// alertsDelivered and alertDeliveryFailures exist because Trigger cannot
	// return an error to its caller (see its doc comment), so these are the
	// only way to tell a working alerting path from a broken one.
	//
	// Worth alerting on in Prometheus, with the obvious caveat: an alert
	// about alerting being down cannot be delivered by the thing that is
	// down. That rule belongs on a route that does not depend on PagerDuty.
	alertsDelivered = promauto.NewCounter(prometheus.CounterOpts{
		Name: "alerting_delivered_total",
		Help: "Alerts successfully accepted by the external alerting provider",
	})
	alertDeliveryFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "alerting_delivery_failures_total",
		Help: "Alerts the external alerting provider did not accept",
	})
)

func logError(err error, eventName string) {
	log.Error().Err(err).Str("event", eventName).
		Msg("alerting: could not deliver alert — the condition it reports is still real")
}
