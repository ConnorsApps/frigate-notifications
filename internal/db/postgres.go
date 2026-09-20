package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/eventstore"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Audit tables are hybrid: columns for filtering/sorting, data for the full
// record.
var postgresSchema = []string{
	`CREATE TABLE IF NOT EXISTS mqtt_events (
		id          bigserial PRIMARY KEY,
		kind        text        NOT NULL,
		received_at timestamptz NOT NULL,
		review_id   text        NOT NULL DEFAULT '',
		event_id    text        NOT NULL DEFAULT '',
		camera      text        NOT NULL DEFAULT '',
		data        jsonb       NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS mqtt_events_received_at_idx ON mqtt_events (received_at)`,
	`CREATE INDEX IF NOT EXISTS mqtt_events_review_idx ON mqtt_events (review_id, event_id)`,

	`CREATE TABLE IF NOT EXISTS notifications (
		id        bigserial PRIMARY KEY,
		sent_at   timestamptz NOT NULL,
		review_id text        NOT NULL DEFAULT '',
		rule      text        NOT NULL DEFAULT '',
		recipient text        NOT NULL DEFAULT '',
		data      jsonb       NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS notifications_sent_at_idx ON notifications (sent_at)`,
	`CREATE INDEX IF NOT EXISTS notifications_review_idx ON notifications (review_id)`,

	`CREATE TABLE IF NOT EXISTS kv (
		key        text PRIMARY KEY,
		value      text        NOT NULL,
		expires_at timestamptz NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS kv_expires_at_idx ON kv (expires_at)`,
}

// janitorInterval: Postgres has no TTL indexes, so a sweep reaps instead.
const janitorInterval = time.Hour

type postgresDB struct {
	pool    *pgxpool.Pool
	tracer  trace.Tracer
	metrics eventstore.Metrics
	logger  zerolog.Logger

	stopJanitor context.CancelFunc
	janitorDone chan struct{}
}

func openPostgres(ctx context.Context, url string, m eventstore.Metrics, o openOptions) (*postgresDB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()
	// State calls sit in the notification path.
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	janitorCtx, stop := context.WithCancel(context.Background())
	p := &postgresDB{
		pool:        pool,
		tracer:      otel.Tracer("frigate-notify/db"),
		metrics:     m,
		logger:      log.With().Str("logger", "db").Str("kind", string(KindPostgres)).Logger(),
		stopJanitor: stop,
		janitorDone: make(chan struct{}),
	}
	if err := otelpgx.RecordStats(pool); err != nil {
		p.logger.Warn().Err(err).Msg("failed to record pool stats")
	}
	if o.readOnly {
		close(p.janitorDone)
		p.logger.Info().Str("db", cfg.ConnConfig.Database).Msg("connected to postgres (read-only)")
		return p, nil
	}

	// Best-effort: without tables, writes just fail and log.
	if err := p.ensureSchema(ctx); err != nil {
		p.logger.Warn().Err(err).Msg("failed to ensure schema; continuing without it")
	}
	go p.janitor(janitorCtx)

	p.logger.Info().Str("db", cfg.ConnConfig.Database).Msg("connected to postgres")
	return p, nil
}

func (p *postgresDB) Close(context.Context) error {
	p.stopJanitor()
	<-p.janitorDone
	p.pool.Close()
	return nil
}

// begin starts a span for op, with an optional timeout. otelpgx only traces
// under an already-recording span, so this is what makes queries visible.
func (p *postgresDB) begin(ctx context.Context, op string, timeout time.Duration) (context.Context, func()) {
	ctx, span := p.tracer.Start(ctx, "db."+op)
	if timeout <= 0 {
		return ctx, func() { span.End() }
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, func() { cancel(); span.End() }
}

func (p *postgresDB) ensureSchema(ctx context.Context) error {
	for _, stmt := range postgresSchema {
		if _, err := p.pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (p *postgresDB) janitor(ctx context.Context) {
	defer close(p.janitorDone)
	p.sweep(ctx)
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweep(ctx)
		}
	}
}

func (p *postgresDB) sweep(ctx context.Context) {
	ctx, end := p.begin(ctx, "sweep", 0)
	defer end()
	retention := eventstore.Retention.Seconds()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM kv WHERE expires_at <= now()`, nil},
		{`DELETE FROM mqtt_events WHERE received_at < now() - make_interval(secs => $1)`, []any{retention}},
		{`DELETE FROM notifications WHERE sent_at < now() - make_interval(secs => $1)`, []any{retention}},
	} {
		if _, err := p.pool.Exec(ctx, q.sql, q.args...); err != nil && ctx.Err() == nil {
			p.logger.Warn().Err(err).Msg("retention sweep failed")
		}
	}
}

// --- eventstore.Store ---

func (p *postgresDB) SaveReviewEvent(_ context.Context, review frigate.ReviewPayload, lifecycle frigate.LifecycleType) {
	rec := eventstore.NewReviewEventRecord(review, lifecycle)
	p.insert("saveReviewEvent",
		`INSERT INTO mqtt_events (kind, received_at, review_id, event_id, camera, data) VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.Kind, rec.ReceivedAt, rec.ReviewID, rec.EventID, rec.Camera, rec)
}

func (p *postgresDB) SaveDescriptionUpdate(_ context.Context, upd frigate.TrackedObjectUpdate) {
	rec := eventstore.NewDescriptionUpdateRecord(upd)
	p.insert("saveDescriptionUpdate",
		`INSERT INTO mqtt_events (kind, received_at, event_id, data) VALUES ($1, $2, $3, $4)`,
		rec.Kind, rec.ReceivedAt, rec.EventID, rec)
}

func (p *postgresDB) SaveNotification(_ context.Context, rec eventstore.NotificationRecord) {
	p.insert("saveNotification",
		`INSERT INTO notifications (sent_at, review_id, rule, recipient, data) VALUES ($1, $2, $3, $4, $5)`,
		rec.SentAt, rec.ReviewID, rec.Rule, rec.Recipient, rec)
}

func (p *postgresDB) insert(op, sql string, args ...any) {
	writeAsync(p.logger, p.metrics, op, func(ctx context.Context) error {
		ctx, end := p.begin(ctx, "audit."+op, 0)
		defer end()
		_, err := p.pool.Exec(ctx, sql, args...)
		return err
	})
}

func (p *postgresDB) Reviews(ctx context.Context, since time.Time, camera string) ([]eventstore.ReviewEventRecord, error) {
	ctx, end := p.begin(ctx, "audit.reviews", 0)
	defer end()
	rows, err := p.pool.Query(ctx,
		`SELECT data FROM mqtt_events
		 WHERE kind = $1 AND received_at >= $2 AND ($3::text = '' OR camera = $3)
		 ORDER BY received_at`,
		eventstore.KindReview, since, camera)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[eventstore.ReviewEventRecord])
}

func (p *postgresDB) CountDescriptions(ctx context.Context, since time.Time) (int64, error) {
	ctx, end := p.begin(ctx, "audit.countDescriptions", 0)
	defer end()
	var n int64
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM mqtt_events WHERE kind = $1 AND received_at >= $2`,
		eventstore.KindDescriptionUpdate, since).Scan(&n)
	return n, err
}

func (p *postgresDB) Notifications(ctx context.Context, since time.Time, rule, recipient string) ([]eventstore.NotificationRecord, error) {
	ctx, end := p.begin(ctx, "audit.notifications", 0)
	defer end()
	rows, err := p.pool.Query(ctx,
		`SELECT data FROM notifications
		 WHERE sent_at >= $1 AND ($2::text = '' OR rule = $2) AND ($3::text = '' OR recipient = $3)
		 ORDER BY sent_at`,
		since, rule, recipient)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[eventstore.NotificationRecord])
}

// --- store.Backend ---
//
// Expiry uses the database clock, so skewed replicas agree.

// SetNX claims key if absent or expired; no row returned means it is held.
func (p *postgresDB) SetNX(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
	ctx, end := p.begin(ctx, "kv.setnx", kvTimeout)
	defer end()
	var one int
	err := p.pool.QueryRow(ctx,
		`INSERT INTO kv AS k (key, value, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at
		 WHERE k.expires_at <= now()
		 RETURNING 1`,
		key, val, ttl.Seconds()).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (p *postgresDB) Get(ctx context.Context, key string) (string, bool, error) {
	ctx, end := p.begin(ctx, "kv.get", kvTimeout)
	defer end()
	var val string
	err := p.pool.QueryRow(ctx,
		`SELECT value FROM kv WHERE key = $1 AND expires_at > now()`, key).Scan(&val)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

func (p *postgresDB) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	ctx, end := p.begin(ctx, "kv.set", kvTimeout)
	defer end()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO kv (key, value, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at`,
		key, val, ttl.Seconds())
	return err
}

func (p *postgresDB) Del(ctx context.Context, key string) error {
	ctx, end := p.begin(ctx, "kv.del", kvTimeout)
	defer end()
	_, err := p.pool.Exec(ctx, `DELETE FROM kv WHERE key = $1`, key)
	return err
}

// Incr is one atomic upsert: expired or new restarts at 1, else +1, and the
// ttl is refreshed.
func (p *postgresDB) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	ctx, end := p.begin(ctx, "kv.incr", kvTimeout)
	defer end()
	var n int64
	err := p.pool.QueryRow(ctx,
		`INSERT INTO kv AS k (key, value, expires_at) VALUES ($1, '1', now() + make_interval(secs => $2))
		 ON CONFLICT (key) DO UPDATE SET
		   value = CASE WHEN k.expires_at <= now() THEN '1' ELSE (k.value::bigint + 1)::text END,
		   expires_at = EXCLUDED.expires_at
		 RETURNING value::bigint`,
		key, ttl.Seconds()).Scan(&n)
	return n, err
}
