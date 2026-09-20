package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// deadBackend stands in for an unreachable Valkey.
type deadBackend struct{ calls int }

var errDead = errors.New("connection refused")

func (d *deadBackend) SetNX(context.Context, string, string, time.Duration) (bool, error) {
	d.calls++
	return false, errDead
}
func (d *deadBackend) Get(context.Context, string) (string, bool, error) {
	d.calls++
	return "", false, errDead
}
func (d *deadBackend) Set(context.Context, string, string, time.Duration) error {
	d.calls++
	return errDead
}
func (d *deadBackend) Del(context.Context, string) error { d.calls++; return errDead }
func (d *deadBackend) Incr(context.Context, string, time.Duration) (int64, error) {
	d.calls++
	return 0, errDead
}

func memStore() *Store {
	return &Store{fallback: newMemBackend(), metrics: nopMetrics{}}
}

func TestFirstDeliverySuppressesRedelivery(t *testing.T) {
	s := memStore()
	ctx := context.Background()

	if !s.FirstDelivery(ctx, "review1", "new") {
		t.Fatal("first delivery should be accepted")
	}
	if s.FirstDelivery(ctx, "review1", "new") {
		t.Error("a QoS 1 redelivery of the same phase should be suppressed")
	}
	// A different phase of the same review is a genuinely new message.
	if !s.FirstDelivery(ctx, "review1", "end") {
		t.Error("a different phase should not be suppressed")
	}
}

func TestAcquireCooldown(t *testing.T) {
	s := memStore()
	ctx := context.Background()

	if !s.AcquireCooldown(ctx, "day:front_porch", time.Minute) {
		t.Fatal("the first send should acquire the cooldown")
	}
	if s.AcquireCooldown(ctx, "day:front_porch", time.Minute) {
		t.Error("a second send inside the window should be blocked")
	}
	if !s.AcquireCooldown(ctx, "day:garage", time.Minute) {
		t.Error("a different scope key should have its own cooldown")
	}
	// A zero cooldown means no gating at all.
	if !s.AcquireCooldown(ctx, "nocooldown", 0) || !s.AcquireCooldown(ctx, "nocooldown", 0) {
		t.Error("a zero cooldown should never block")
	}
}

func TestCooldownExpires(t *testing.T) {
	s := memStore()
	ctx := context.Background()

	if !s.AcquireCooldown(ctx, "k", 20*time.Millisecond) {
		t.Fatal("first acquire failed")
	}
	time.Sleep(40 * time.Millisecond)
	if !s.AcquireCooldown(ctx, "k", 20*time.Millisecond) {
		t.Error("the cooldown should have expired")
	}
}

func TestReviewStateRoundTrip(t *testing.T) {
	s := memStore()
	ctx := context.Background()

	if _, ok := s.LoadReview(ctx, "missing"); ok {
		t.Error("an unknown review should not be found")
	}

	want := ReviewState{RuleIndex: 2, RuleName: "day", Tag: "fn-r1", Deliveries: []Delivery{{Recipient: "alice", Target: 1, Ref: "C1:1.2"}}, EventID: "evt1"}
	s.SaveReview(ctx, "r1", want)

	got, ok := s.LoadReview(ctx, "r1")
	if !ok {
		t.Fatal("saved review should be found")
	}
	if got.RuleIndex != want.RuleIndex || got.Tag != want.Tag || got.EventID != want.EventID ||
		len(got.Deliveries) != 1 || got.Deliveries[0] != want.Deliveries[0] {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestCountForHourRollsOver(t *testing.T) {
	s := memStore()
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 14, 30, 0, 0, time.UTC)

	for i := int64(1); i <= 3; i++ {
		if got := s.CountForHour(ctx, "alice", now); got != i {
			t.Errorf("count = %d, want %d", got, i)
		}
	}
	// The next hour starts a fresh count.
	if got := s.CountForHour(ctx, "alice", now.Add(time.Hour)); got != 1 {
		t.Errorf("count in the next hour = %d, want 1", got)
	}
	// So does a different recipient.
	if got := s.CountForHour(ctx, "bob", now); got != 1 {
		t.Errorf("count for another recipient = %d, want 1", got)
	}
}

func TestDigestAccumulates(t *testing.T) {
	s := memStore()
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 2, 15, 0, 0, time.UTC)

	first := s.AddToDigest(ctx, "alice", "front_porch", now)
	if first.Count != 1 {
		t.Fatalf("count = %d, want 1", first.Count)
	}

	second := s.AddToDigest(ctx, "alice", "front_porch", now.Add(time.Minute))
	if second.Count != 2 {
		t.Errorf("count = %d, want 2", second.Count)
	}
	// The digest reports when the suppressed run started, not the last event.
	if second.SinceTS != first.SinceTS {
		t.Error("the digest should keep its original start time")
	}

	s.ClearDigest(ctx, "alice", "front_porch")
	if got := s.AddToDigest(ctx, "alice", "front_porch", now); got.Count != 1 {
		t.Errorf("count after clear = %d, want 1", got.Count)
	}
}

// The single most important property in this package: with Valkey down, every
// operation must still yield an answer that produces MORE notifications, not
// fewer. A security system that mutes itself when its cache dies is worse
// than one with no cache at all.
func TestFailsOpenWhenPrimaryIsDown(t *testing.T) {
	dead := &deadBackend{}
	fallbacks := map[string]int{}
	s := &Store{
		primary:  dead,
		fallback: newMemBackend(),
		metrics:  metricsFunc(func(op string) { fallbacks[op]++ }),
	}
	ctx := context.Background()

	if !s.FirstDelivery(ctx, "r1", "new") {
		t.Error("a dead store must not suppress a review as a redelivery")
	}
	if !s.AcquireCooldown(ctx, "k", time.Minute) {
		t.Error("a dead store must not suppress a send as cooled down")
	}
	if got := s.CountForHour(ctx, "alice", time.Now()); got > 1 {
		t.Errorf("count = %d; a dead store must not read as over the rate cap", got)
	}

	if dead.calls == 0 {
		t.Error("the primary should have been attempted")
	}
	if len(fallbacks) == 0 {
		t.Error("falling back should be recorded so a dead Valkey is visible in metrics")
	}
}

type metricsFunc func(op string)

func (f metricsFunc) StoreFallback(op string) { f(op) }

// With no Valkey url the WithBackend database is primary, with the same
// fail-open fallback.
func TestWithBackendIsPrimaryWhenRedisUnset(t *testing.T) {
	kv := newMemBackend()
	s := New(context.Background(), "", nil, WithBackend(kv))
	ctx := context.Background()

	if !s.AcquireCooldown(ctx, "k", time.Minute) {
		t.Fatal("first acquire should succeed")
	}
	if _, ok, _ := kv.Get(ctx, "fn:cooldown:k"); !ok {
		t.Error("the cooldown should have been written to the supplied backend")
	}

	dead := &deadBackend{}
	s = New(context.Background(), "", nil, WithBackend(dead))
	if !s.FirstDelivery(ctx, "r1", "new") {
		t.Error("a dead database must not suppress a review")
	}
	if dead.calls == 0 {
		t.Error("the supplied backend should have been attempted")
	}
	if s.FirstDelivery(ctx, "r1", "new") {
		t.Error("the in-memory fallback should still catch the redelivery")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
