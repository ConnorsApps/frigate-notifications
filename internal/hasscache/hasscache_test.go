package hasscache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// reader is a StateReader whose answer and latency a test controls.
type reader struct {
	calls atomic.Int32
	hang  bool
	err   error
	body  string
}

func (r *reader) EntityState(ctx context.Context, _ string) ([]byte, error) {
	r.calls.Add(1)
	if r.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, r.err
	}
	return []byte(r.body), nil
}

func shorten(t *testing.T, lookup, fail time.Duration) {
	t.Helper()
	oldLookup, oldFail := lookupTimeout, failTTL
	lookupTimeout, failTTL = lookup, fail
	t.Cleanup(func() { lookupTimeout, failTTL = oldLookup, oldFail })
}

// A Home Assistant that never answers must not hang a caller whose context
// has no deadline — that caller is the MQTT dispatch goroutine.
func TestHungHomeAssistantTimesOut(t *testing.T) {
	shorten(t, 50*time.Millisecond, time.Minute)
	c := New(&reader{hang: true}, time.UTC)

	done := make(chan error, 1)
	go func() { _, err := c.State(context.Background(), "alarm_control_panel.home"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a hung read should report an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("State did not return: a hung Home Assistant blocks the caller")
	}

	done = make(chan error, 1)
	go func() { _, err := c.Solar(context.Background()); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Solar did not return: a hung Home Assistant blocks the caller")
	}
}

// While Home Assistant is down, a burst of reviews shares one failed read
// instead of paying a timeout each.
func TestFailedReadIsRemembered(t *testing.T) {
	shorten(t, time.Second, time.Minute)
	r := &reader{err: errors.New("down")}
	c := New(r, time.UTC)

	for range 5 {
		if _, err := c.State(context.Background(), "input_boolean.guest_mode"); err == nil {
			t.Fatal("want the read error")
		}
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("Home Assistant was asked %d times, want 1", got)
	}

	for range 5 {
		_, _ = c.Solar(context.Background())
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("sun.sun was asked %d times in total, want 1 more", got-1)
	}
}

// The failure memory is short: once it lapses the next read goes out, and a
// recovered Home Assistant is believed again.
func TestFailureMemoryLapses(t *testing.T) {
	shorten(t, time.Second, 20*time.Millisecond)
	r := &reader{err: errors.New("down")}
	c := New(r, time.UTC)

	if _, err := c.State(context.Background(), "x.y"); err == nil {
		t.Fatal("want the read error")
	}
	r.err, r.body = nil, `{"state":"on"}`
	time.Sleep(40 * time.Millisecond)

	got, err := c.State(context.Background(), "x.y")
	if err != nil || got != "on" {
		t.Errorf("State = %q, %v; want on, nil once the failure memory lapsed", got, err)
	}
}

// A canceled caller (shutdown) says nothing about Home Assistant.
func TestCanceledCallerIsNotRemembered(t *testing.T) {
	shorten(t, time.Second, time.Minute)
	r := &reader{hang: true}
	c := New(r, time.UTC)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.State(ctx, "x.y"); err == nil {
		t.Fatal("want an error from a canceled context")
	}

	r.hang, r.body = false, `{"state":"off"}`
	if got, err := c.State(context.Background(), "x.y"); err != nil || got != "off" {
		t.Errorf("State = %q, %v; want off, nil: a canceled read must not poison the cache", got, err)
	}
}
