// Package metrics holds the OTel counters for notification decisions.
//
// Every attribute here is drawn from config (rule, recipient, preset) or a
// closed set (reason, phase). Review and event ids are deliberately never
// used as attributes: they are unbounded, and one notification storm would
// turn into a cardinality incident on the metrics backend.
package metrics

import (
	"context"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type Metrics struct {
	reviews     metric.Int64Counter
	sent        metric.Int64Counter
	suppressed  metric.Int64Counter
	unmatched   metric.Int64Counter
	notifyError metric.Int64Counter
	fallback    metric.Int64Counter
	mediaServed metric.Int64Counter
	mediaPicked metric.Int64Counter
	mediaReject metric.Int64Counter
	esWrites    metric.Int64Counter
}

func New() *Metrics {
	meter := otel.Meter("frigate-notify")
	var err error
	ctr := func(name, desc string) metric.Int64Counter {
		c, e := meter.Int64Counter("frigate_notify_"+name, metric.WithDescription(desc))
		if e != nil {
			err = e
		}
		return c
	}

	m := &Metrics{
		reviews:     ctr("reviews_total", "Frigate review messages received, by phase"),
		sent:        ctr("sent_total", "Notifications sent, by rule, recipient, backend and preset"),
		suppressed:  ctr("suppressed_total", "Notifications suppressed, by reason"),
		unmatched:   ctr("unmatched_total", "Reviews that matched no rule, by camera"),
		notifyError: ctr("errors_total", "Notification delivery failures, by recipient and backend"),
		fallback:    ctr("store_fallback_total", "State store operations that fell back to memory, by op"),
		mediaServed: ctr("media_served_total", "Media proxy responses served, by kind"),
		mediaPicked: ctr("media_selected_total", "Attachment each notification carried, by phase and kind (none = every candidate failed, off = text preset)"),
		mediaReject: ctr("media_rejected_total", "Media proxy requests rejected, by reason"),
		esWrites:    ctr("eventstore_writes_total", "Audit event-store writes, by op and result (ok|error)"),
	}
	if err != nil {
		log.Warn().Err(err).Msg("failed to create some metrics instruments")
	}
	return m
}

// add increments c, with attributes given as alternating keys and values.
func add(c metric.Int64Counter, kv ...string) {
	attrs := make([]attribute.KeyValue, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		attrs = append(attrs, attribute.String(kv[i], kv[i+1]))
	}
	c.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

func (m *Metrics) ReviewReceived(phase, camera string) {
	add(m.reviews, "phase", phase, "camera", camera)
}

func (m *Metrics) Sent(rule, recipient, backend, preset, phase string) {
	add(m.sent, "rule", rule, "recipient", recipient, "backend", backend, "preset", preset, "phase", phase)
}

// Suppressed records a notification that a rule selected but policy dropped.
// reason is a closed set: cooldown, outside_active_hours, rate_cap.
func (m *Metrics) Suppressed(reason, rule, recipient string) {
	add(m.suppressed, "reason", reason, "rule", rule, "recipient", recipient)
}

func (m *Metrics) Unmatched(camera, phase string) {
	add(m.unmatched, "camera", camera, "phase", phase)
}

func (m *Metrics) NotifyError(recipient, backend string) {
	add(m.notifyError, "recipient", recipient, "backend", backend)
}

// StoreFallback implements store.Metrics.
func (m *Metrics) StoreFallback(op string) { add(m.fallback, "op", op) }

// EventStoreWrite implements eventstore.Metrics. result is "ok" or "error".
func (m *Metrics) EventStoreWrite(op, result string) { add(m.esWrites, "op", op, "result", result) }

// MediaSelected implements notifier.Metrics.
func (m *Metrics) MediaSelected(phase, kind string) { add(m.mediaPicked, "phase", phase, "kind", kind) }

// MediaServed implements media.Metrics.
func (m *Metrics) MediaServed(kind string) { add(m.mediaServed, "kind", kind) }

// MediaRejected implements media.Metrics.
func (m *Metrics) MediaRejected(reason string) { add(m.mediaReject, "reason", reason) }
