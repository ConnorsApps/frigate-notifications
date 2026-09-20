// Package db is the MongoDB or PostgreSQL database, chosen by URL scheme. One
// connection is the audit log (eventstore.Store) and, when no Valkey is
// configured, the decision-state store (store.Backend).
package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/eventstore"
	"github.com/ConnorsApps/frigate-notifications/internal/store"
	"github.com/rs/zerolog"
)

// Kind names a supported database.
type Kind string

const (
	KindMongo    Kind = "mongo"
	KindPostgres Kind = "postgres"
)

// KindOf reports which database a URL points at, from its scheme.
func KindOf(url string) (Kind, error) {
	const want = "want mongodb://, mongodb+srv://, postgres:// or postgresql://"
	scheme, _, ok := strings.Cut(url, "://")
	if !ok {
		return "", fmt.Errorf("missing scheme (%s)", want)
	}
	switch strings.ToLower(scheme) {
	case "mongodb", "mongodb+srv":
		return KindMongo, nil
	case "postgres", "postgresql":
		return KindPostgres, nil
	}
	return "", fmt.Errorf("unsupported scheme %q (%s)", scheme, want)
}

// DB is an open database connection.
type DB interface {
	eventstore.Store
	store.Backend

	Close(ctx context.Context) error
}

// Option customises Open.
type Option func(*openOptions)

type openOptions struct{ readOnly bool }

// ReadOnly skips schema/index creation and retention sweeps.
func ReadOnly() Option { return func(o *openOptions) { o.readOnly = true } }

// Open connects to the database at url and ensures its schema. Callers should
// treat an error as "run without persistence", not a fatal startup failure.
func Open(ctx context.Context, url string, m eventstore.Metrics, opts ...Option) (DB, error) {
	kind, err := KindOf(url)
	if err != nil {
		return nil, err
	}
	if m == nil {
		m = nopMetrics{}
	}
	var o openOptions
	for _, opt := range opts {
		opt(&o)
	}
	// Return concrete types only on success: a nil *T in the interface would
	// compare non-nil.
	if kind == KindMongo {
		d, err := openMongo(ctx, url, m, o)
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	d, err := openPostgres(ctx, url, m, o)
	if err != nil {
		return nil, err
	}
	return d, nil
}

type nopMetrics struct{}

func (nopMetrics) EventStoreWrite(string, string) {}

const (
	writeTimeout = 3 * time.Second
	// kvTimeout is short: state calls run on the MQTT dispatch goroutine and
	// the fallback (forget a cooldown) is cheap.
	kvTimeout = 2 * time.Second
)

// writeAsync runs write in a detached goroutine. It must never block the
// caller: audit writes come from the MQTT dispatch goroutine, and a slow
// database would stall every subsequent Frigate event.
func writeAsync(logger zerolog.Logger, m eventstore.Metrics, op string, write func(ctx context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		if err := write(ctx); err != nil {
			logger.Warn().Err(err).Str("op", op).Msg("event store write failed")
			m.EventStoreWrite(op, "error")
			return
		}
		m.EventStoreWrite(op, "ok")
	}()
}
