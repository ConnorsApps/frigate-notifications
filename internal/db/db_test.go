package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/eventstore"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestKindOf(t *testing.T) {
	tests := []struct {
		url  string
		want Kind
		err  bool
	}{
		{"mongodb://h/d", KindMongo, false},
		{"mongodb+srv://h/d", KindMongo, false},
		{"postgres://u@h/d", KindPostgres, false},
		{"postgresql://u@h/d", KindPostgres, false},
		{"POSTGRES://u@h/d", KindPostgres, false},
		{"mysql://u@h/d", "", true},
		{"localhost:5432", "", true},
		{"", "", true},
	}
	for _, tc := range tests {
		got, err := KindOf(tc.url)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("KindOf(%q) = %q, %v; want %q (err=%v)", tc.url, got, err, tc.want, tc.err)
		}
	}
}

// Contract tests need a live database; skipped without the env var.
func TestPostgres(t *testing.T) { runContract(t, "TEST_POSTGRES_URL") }
func TestMongo(t *testing.T)    { runContract(t, "TEST_MONGO_URL") }

func runContract(t *testing.T, envVar string) {
	url := os.Getenv(envVar)
	if url == "" {
		t.Skipf("%s not set", envVar)
	}
	ctx := context.Background()
	d, err := Open(ctx, url, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close(ctx) })

	// Shared database: keep keys/names unique per run.
	uniq := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())

	t.Run("SetNX", func(t *testing.T) {
		key := "setnx:" + uniq
		if ok, err := d.SetNX(ctx, key, "1", time.Minute); err != nil || !ok {
			t.Fatalf("first SetNX = %v, %v; want true", ok, err)
		}
		if ok, err := d.SetNX(ctx, key, "2", time.Minute); err != nil || ok {
			t.Fatalf("second SetNX = %v, %v; want false while held", ok, err)
		}
		if v, ok, err := d.Get(ctx, key); err != nil || !ok || v != "1" {
			t.Errorf("Get = %q, %v, %v; want the first value", v, ok, err)
		}
	})

	t.Run("SetNXReacquiresAfterExpiry", func(t *testing.T) {
		key := "expiry:" + uniq
		if ok, err := d.SetNX(ctx, key, "1", 300*time.Millisecond); err != nil || !ok {
			t.Fatalf("SetNX = %v, %v", ok, err)
		}
		time.Sleep(600 * time.Millisecond)
		if _, ok, err := d.Get(ctx, key); err != nil || ok {
			t.Errorf("Get after expiry = %v, %v; want absent", ok, err)
		}
		if ok, err := d.SetNX(ctx, key, "2", time.Minute); err != nil || !ok {
			t.Errorf("SetNX after expiry = %v, %v; want true", ok, err)
		}
	})

	t.Run("SetGetDel", func(t *testing.T) {
		key := "set:" + uniq
		if _, ok, err := d.Get(ctx, key); err != nil || ok {
			t.Fatalf("Get of a missing key = %v, %v", ok, err)
		}
		if err := d.Set(ctx, key, "a", time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := d.Set(ctx, key, "b", time.Minute); err != nil {
			t.Fatal(err)
		}
		if v, ok, err := d.Get(ctx, key); err != nil || !ok || v != "b" {
			t.Errorf("Get = %q, %v, %v; want the overwritten value", v, ok, err)
		}
		if err := d.Del(ctx, key); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := d.Get(ctx, key); ok {
			t.Error("key should be gone after Del")
		}
	})

	t.Run("Incr", func(t *testing.T) {
		key := "incr:" + uniq
		for want := int64(1); want <= 3; want++ {
			if got, err := d.Incr(ctx, key, time.Minute); err != nil || got != want {
				t.Fatalf("Incr = %d, %v; want %d", got, err, want)
			}
		}
	})

	t.Run("IncrRestartsAfterExpiry", func(t *testing.T) {
		key := "incr-expiry:" + uniq
		if _, err := d.Incr(ctx, key, 300*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if got, _ := d.Incr(ctx, key, 300*time.Millisecond); got != 2 {
			t.Fatalf("second Incr = %d, want 2", got)
		}
		time.Sleep(600 * time.Millisecond)
		if got, err := d.Incr(ctx, key, time.Minute); err != nil || got != 1 {
			t.Errorf("Incr after expiry = %d, %v; want 1", got, err)
		}
	})

	t.Run("Audit", func(t *testing.T) {
		camera, rule, recipient := "cam-"+uniq, "rule-"+uniq, "who-"+uniq
		since := time.Now().Add(-time.Minute)
		end := 12.5

		d.SaveReviewEvent(ctx, frigate.ReviewPayload{
			ID: "review-" + uniq, Camera: camera, Severity: "alert", EndTime: &end,
			Data: frigate.ReviewData{Detections: []string{"evt-" + uniq}, Objects: []string{"person"}},
		}, "end")
		d.SaveDescriptionUpdate(ctx, frigate.TrackedObjectUpdate{ID: "evt-" + uniq, Description: "a person"})
		d.SaveNotification(ctx, eventstore.NotificationRecord{
			SentAt: time.Now(), ReviewID: "review-" + uniq, Rule: rule, Recipient: recipient,
			Success: true, Title: "hi",
		})

		// Writes are async; poll.
		var (
			reviews []eventstore.ReviewEventRecord
			notes   []eventstore.NotificationRecord
			descs   int64
		)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			reviews, _ = d.Reviews(ctx, since, camera)
			notes, _ = d.Notifications(ctx, since, rule, recipient)
			descs, _ = d.CountDescriptions(ctx, since)
			if len(reviews) == 1 && len(notes) == 1 && descs >= 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		if len(reviews) != 1 {
			t.Fatalf("Reviews = %d records, want 1", len(reviews))
		}
		r := reviews[0]
		if r.Kind != eventstore.KindReview || r.ReviewID != "review-"+uniq || r.EventID != "evt-"+uniq ||
			r.LifecycleType != "end" || r.EndTime == nil || *r.EndTime != end || len(r.Objects) != 1 {
			t.Errorf("review did not round-trip: %+v", r)
		}
		if len(notes) != 1 || notes[0].Title != "hi" || !notes[0].Success {
			t.Errorf("Notifications = %+v, want the one saved", notes)
		}
		if descs < 1 {
			t.Error("CountDescriptions should count the saved update")
		}

		if got, _ := d.Reviews(ctx, since, "no-such-camera"); len(got) != 0 {
			t.Errorf("camera filter leaked %d records", len(got))
		}
		if got, _ := d.Notifications(ctx, since, "no-such-rule", ""); len(got) != 0 {
			t.Errorf("rule filter leaked %d records", len(got))
		}
		if got, _ := d.Notifications(ctx, time.Now().Add(time.Hour), rule, ""); len(got) != 0 {
			t.Errorf("since filter leaked %d records", len(got))
		}
	})
}

func TestPostgresSweep(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL not set")
	}
	ctx := context.Background()
	p, err := openPostgres(ctx, url, nopMetrics{}, openOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(ctx)

	uniq := fmt.Sprintf("sweep-%d", time.Now().UnixNano())
	old := time.Now().Add(-eventstore.Retention - time.Hour)
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO kv VALUES ('%s-dead', 'x', now() - interval '1 minute')`, uniq),
		fmt.Sprintf(`INSERT INTO kv VALUES ('%s-live', 'x', now() + interval '1 hour')`, uniq),
	} {
		if _, err := p.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO notifications (sent_at, review_id, data) VALUES ($1, $2, '{}')`, old, uniq); err != nil {
		t.Fatal(err)
	}

	p.sweep(ctx)

	var kvLeft, notesLeft int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM kv WHERE key LIKE $1`, uniq+"-%").Scan(&kvLeft); err != nil {
		t.Fatal(err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE review_id = $1`, uniq).Scan(&notesLeft); err != nil {
		t.Fatal(err)
	}
	if kvLeft != 1 {
		t.Errorf("kv rows left = %d, want only the live one", kvLeft)
	}
	if notesLeft != 0 {
		t.Errorf("past-retention notification survived the sweep")
	}
}

// Both backends must emit spans through the global tracer provider.
func TestTracing(t *testing.T) {
	for _, envVar := range []string{"TEST_POSTGRES_URL", "TEST_MONGO_URL"} {
		t.Run(envVar, func(t *testing.T) {
			url := os.Getenv(envVar)
			if url == "" {
				t.Skipf("%s not set", envVar)
			}
			rec := tracetest.NewSpanRecorder()
			prev := otel.GetTracerProvider()
			otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
			t.Cleanup(func() { otel.SetTracerProvider(prev) })

			ctx := context.Background()
			d, err := Open(ctx, url, nil, ReadOnly())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close(ctx)
			if _, _, err := d.Get(ctx, "trace-probe"); err != nil {
				t.Fatal(err)
			}
			// Postgres has our op span plus at least one otelpgx child.
			want := 1
			if envVar == "TEST_POSTGRES_URL" {
				want = 2
			}
			if got := len(rec.Ended()); got < want {
				t.Errorf("spans = %d, want >= %d", got, want)
			}
		})
	}
}
