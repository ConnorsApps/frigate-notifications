// Package store holds the cross-restart state the notifier needs: cooldowns,
// per-review notification bookkeeping, rate caps, and the MQTT redelivery
// guard. CI restarts this deployment on every image push, so in-process state
// would drop every live cooldown and every pending "update on end".
//
// It is backed by Valkey when configured, else the database from WithBackend.
// Every operation fails OPEN: on a primary error it falls back to an
// in-process map, which at worst costs an extra notification. No operation may
// ever suppress because of an infrastructure error.
package store

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Backend is the key/value surface. Keys expire after ttl; Incr refreshes it.
type Backend interface {
	SetNX(ctx context.Context, key, val string, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, val string, ttl time.Duration) error
	Del(ctx context.Context, key string) error
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// ReviewState is what we remember of a notified review, so later phases edit
// the same notification.
type ReviewState struct {
	RuleIndex  int        `json:"ruleIndex"`
	RuleName   string     `json:"ruleName"`
	Tag        string     `json:"tag"`
	Deliveries []Delivery `json:"deliveries"`
	Critical   bool       `json:"critical"`
	EventID    string     `json:"eventId"`
}

// Delivery is one successful send: recipient, target (index into
// Recipient.Targets), and the message's ref.
type Delivery struct {
	Recipient string `json:"recipient"`
	Target    int    `json:"target"`
	// Ref is opaque to the store; "" for backends that replace by tag.
	Ref string `json:"ref,omitempty"`
}

// Recipients returns the distinct recipients, in first-delivered order.
func (s ReviewState) Recipients() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range s.Deliveries {
		if !seen[d.Recipient] {
			seen[d.Recipient] = true
			out = append(out, d.Recipient)
		}
	}
	return out
}

// DigestEntry accumulates what a recipient was not told about.
type DigestEntry struct {
	Count   int   `json:"count"`
	SinceTS int64 `json:"since"`
}

// Store is the notifier's view of persistent state.
type Store struct {
	primary  Backend
	fallback Backend
	closer   io.Closer // the Valkey client, if opened
	metrics  Metrics
	logger   zerolog.Logger
}

// Metrics is the observability hook; kept as an interface so this package
// carries no OTel dependency.
type Metrics interface {
	StoreFallback(op string)
}

type nopMetrics struct{}

func (nopMetrics) StoreFallback(string) {}

// Option customises New.
type Option func(*options)

type options struct{ kv Backend }

// WithBackend supplies the primary used when no Valkey url is set. The caller
// keeps ownership; Store.Close won't close it.
func WithBackend(kv Backend) Option { return func(o *options) { o.kv = kv } }

// New returns a store backed by Valkey at url, else the WithBackend database,
// else memory. A startup connection failure is not fatal: state just doesn't
// survive restarts.
func New(ctx context.Context, url string, m Metrics, opts ...Option) *Store {
	if m == nil {
		m = nopMetrics{}
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	s := &Store{
		fallback: newMemBackend(),
		metrics:  m,
		logger:   log.With().Str("logger", "store").Logger(),
	}
	switch {
	case url != "":
		rb, err := newRedisBackend(ctx, url)
		if err != nil {
			s.logger.Error().Err(err).Msg("redis unavailable at startup; continuing with in-memory state")
			return s
		}
		s.primary = rb
		s.closer = rb
	case o.kv != nil:
		s.primary = o.kv
	default:
		s.logger.Warn().Msg("no redis url or db configured; state is in-memory and will not survive a restart")
	}
	return s
}

// Close releases the Valkey client, if this store opened one.
func (s *Store) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// do runs fn on the primary and falls back to memory on error.
func (s *Store) do(op string, fn func(Backend) error) {
	if s.primary != nil {
		err := fn(s.primary)
		if err == nil {
			return
		}
		s.logger.Warn().Err(err).Str("op", op).Msg("primary store operation failed; falling back to in-memory state")
		s.metrics.StoreFallback(op)
	}
	if err := fn(s.fallback); err != nil {
		// The in-memory backend cannot fail; this is unreachable in practice.
		s.logger.Error().Err(err).Str("op", op).Msg("in-memory store operation failed")
	}
}

// setNX reports whether key was newly set. Fails open: on error it reports
// true, so a lost store costs a duplicate, never a suppression.
func (s *Store) setNX(ctx context.Context, op, key string, ttl time.Duration) bool {
	first := true
	s.do(op, func(b Backend) error {
		ok, err := b.SetNX(ctx, key, "1", ttl)
		if err == nil {
			first = ok
		}
		return err
	})
	return first
}

func (s *Store) get(ctx context.Context, op, key string) (v string, found bool) {
	s.do(op, func(b Backend) error {
		var err error
		v, found, err = b.Get(ctx, key)
		return err
	})
	return v, found
}

func (s *Store) set(ctx context.Context, op, key, val string, ttl time.Duration) {
	s.do(op, func(b Backend) error { return b.Set(ctx, key, val, ttl) })
}

// FirstDelivery reports whether this (review, phase) has not been handled yet.
// MQTT is QoS 1, so the broker may redeliver the same payload.
//
// Fails open: on any error it returns true and the notification is processed
// again, which risks a duplicate push rather than a missed one.
func (s *Store) FirstDelivery(ctx context.Context, reviewID, phase string) bool {
	return s.setNX(ctx, "seen", "fn:seen:"+reviewID+":"+phase, 10*time.Minute)
}

// AcquireCooldown reports whether the caller may send now, starting a new
// cooldown window if so. Gates first sends only, never end-phase updates.
//
// Fails open: an unreachable store yields an extra notification, not silence.
func (s *Store) AcquireCooldown(ctx context.Context, scopeKey string, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	return s.setNX(ctx, "cooldown", "fn:cooldown:"+scopeKey, ttl)
}

// SaveReview records what was sent for a review so a later phase can update
// or escalate it.
func (s *Store) SaveReview(ctx context.Context, reviewID string, st ReviewState) {
	blob, err := json.Marshal(st)
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to marshal review state")
		return
	}
	s.set(ctx, "saveReview", "fn:review:"+reviewID, string(blob), 6*time.Hour)
}

// LoadReview returns what was sent for a review, if anything.
func (s *Store) LoadReview(ctx context.Context, reviewID string) (ReviewState, bool) {
	var st ReviewState
	found := false
	s.do("loadReview", func(b Backend) error {
		raw, ok, err := b.Get(ctx, "fn:review:"+reviewID)
		if err != nil {
			return err
		}
		if !ok {
			found = false
			return nil
		}
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return err
		}
		found = true
		return nil
	})
	return st, found
}

// CountForHour increments and returns a recipient's push count for the
// current hour. Returns 0 on failure, which reads as "under the cap".
func (s *Store) CountForHour(ctx context.Context, recipient string, now time.Time) int64 {
	var n int64
	s.do("rate", func(b Backend) error {
		key := "fn:rate:" + recipient + ":" + now.Format("2006010215")
		v, err := b.Incr(ctx, key, 2*time.Hour)
		if err != nil {
			return err
		}
		n = v
		return nil
	})
	return n
}

// AddToDigest records an event a recipient was not pushed about.
func (s *Store) AddToDigest(ctx context.Context, recipient, camera string, now time.Time) DigestEntry {
	key := "fn:digest:" + recipient + ":" + camera
	entry := DigestEntry{SinceTS: now.Unix()}
	s.do("digest", func(b Backend) error {
		raw, ok, err := b.Get(ctx, key)
		if err != nil {
			return err
		}
		if ok {
			var prev DigestEntry
			if err := json.Unmarshal([]byte(raw), &prev); err == nil && prev.SinceTS > 0 {
				entry.SinceTS = prev.SinceTS
				entry.Count = prev.Count
			}
		}
		entry.Count++
		blob, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return b.Set(ctx, key, string(blob), 2*time.Hour)
	})
	return entry
}

// ClearDigest drops a recipient's accumulated digest for a camera.
func (s *Store) ClearDigest(ctx context.Context, recipient, camera string) {
	s.do("clearDigest", func(b Backend) error {
		return b.Del(ctx, "fn:digest:"+recipient+":"+camera)
	})
}

// SetDescription caches a GenAI object description keyed by Frigate event id.
// Descriptions arrive on their own topic, usually after the review ends.
func (s *Store) SetDescription(ctx context.Context, eventID, desc string) {
	s.set(ctx, "setDesc", "fn:desc:"+eventID, desc, time.Hour)
}

// Description returns a cached GenAI description, or "" if none arrived.
func (s *Store) Description(ctx context.Context, eventID string) string {
	desc, _ := s.get(ctx, "getDesc", "fn:desc:"+eventID)
	return desc
}
