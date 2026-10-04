package notifier

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
)

// A delivery that failed outright must not leave the cooldown running: the
// next review, after the backend recovers, is the one that should be heard.
func TestFailedDeliveryDoesNotStartTheCooldown(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	cfg.Rules[0].Cooldown = config.Duration(5 * time.Minute)
	n, hass := newTestNotifier(t, cfg)
	ctx := context.Background()

	hass.fail = true
	n.HandleReview(ctx, loadReview(t, "review-new.json"))

	hass.mu.Lock()
	hass.fail = false
	hass.mu.Unlock()
	second := loadReview(t, "review-new.json")
	second.After.ID = "a-second-review"
	n.HandleReview(ctx, second)

	if hass.count() != 1 {
		t.Fatalf("sent %d notifications, want the second review's 1: a failed send must not start the cooldown", hass.count())
	}

	// And once something was delivered, the cooldown holds as before.
	third := loadReview(t, "review-new.json")
	third.After.ID = "a-third-review"
	n.HandleReview(ctx, third)
	if hass.count() != 1 {
		t.Errorf("sent %d notifications, want the third held by the cooldown the second started", hass.count())
	}
}

// Nothing to send because every recipient is outside their active hours is
// not a send either.
func TestPolicyHeldBackReviewDoesNotStartTheCooldown(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	cfg.Rules[0].Cooldown = config.Duration(5 * time.Minute)
	alice := cfg.Recipients["alice"]
	alice.AllowCritical = false
	alice.ActiveHours = &config.Window{From: endpoint(t, "03:00"), To: endpoint(t, "04:00")}
	cfg.Recipients["alice"] = alice
	n, hass := newTestNotifier(t, cfg)

	asleep := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	n.now = func() time.Time { return asleep }
	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if hass.count() != 0 {
		t.Fatal("alice is outside her active hours, nothing should be sent")
	}

	awake := time.Date(2026, 1, 1, 3, 30, 0, 0, time.UTC)
	n.now = func() time.Time { return awake }
	second := loadReview(t, "review-new.json")
	second.After.ID = "a-second-review"
	n.HandleReview(context.Background(), second)
	if hass.count() != 1 {
		t.Errorf("sent %d, want 1: a review nobody was told about must not hold off the next", hass.count())
	}
}

// escalationConfig has a critical rule for a zone above a catch-all, with bob
// unable to receive critical.
func escalationConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	zero := config.Duration(0)
	bob := cfg.Recipients["bob"]
	bob.AllowCritical = false
	cfg.Recipients["bob"] = bob
	cfg.Rules = []config.Rule{
		{Name: "person-in-zone", When: config.RuleConditions{Zones: []string{"back_porch"}}, To: []string{"alice", "bob"}, Preset: config.PresetAuto, Critical: true},
		{Name: "any-person", When: config.RuleConditions{Labels: []string{"person"}}, Holdoff: &zero, To: []string{"alice", "bob"}, Preset: config.PresetAuto},
	}
	return cfg
}

func escalate(t *testing.T, n *Notifier) {
	t.Helper()
	ctx := context.Background()
	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	update := loadReview(t, "review-new.json")
	update.Type = frigate.LifecycleUpdate
	update.After.Data.Zones = []string{"back_porch"}
	n.HandleReview(ctx, update)
}

func tagOf(s sentNotification) any { return s.payload["data"].(map[string]any)["tag"] }

// A recipient who can't receive critical already has the quiet push; the
// escalation must not give them a second, identical one.
func TestEscalationSkipsRecipientsWhoCannotReceiveCritical(t *testing.T) {
	n, hass := newTestNotifier(t, escalationConfig(t))
	escalate(t, n)

	var alice, bob []sentNotification
	for _, s := range hass.all() {
		switch s.service {
		case "mobile_app_alice":
			alice = append(alice, s)
		case "mobile_app_bob":
			bob = append(bob, s)
		}
	}
	if len(alice) != 2 || !isCritical(alice[1]) {
		t.Errorf("alice got %d pushes, want the original and a critical escalation", len(alice))
	}
	if len(bob) != 1 {
		t.Errorf("bob (allowCritical: false) got %d pushes, want only the original", len(bob))
	}
}

// The end of the review still updates what each recipient actually has: bob's
// quiet notification under its original tag, alice's under the escalation's.
func TestEndUpdatesEveryRecipientAfterAnEscalation(t *testing.T) {
	n, hass := newTestNotifier(t, escalationConfig(t))
	escalate(t, n)
	before := hass.count()

	n.HandleReview(context.Background(), loadReview(t, "review-end.json"))

	updates := hass.all()[before:]
	tags := map[string]any{}
	for _, s := range updates {
		tags[s.service] = tagOf(s)
	}
	if len(updates) != 2 {
		t.Fatalf("end sent %d updates, want one per recipient: %v", len(updates), tags)
	}
	if tags["mobile_app_alice"] != "fn-1787614651.122803-w0zlfu-esc" {
		t.Errorf("alice's update tag = %v, want the escalation's", tags["mobile_app_alice"])
	}
	if tags["mobile_app_bob"] != "fn-1787614651.122803-w0zlfu" {
		t.Errorf("bob's update tag = %v, want his original notification's so it is edited, not duplicated", tags["mobile_app_bob"])
	}
}

// A cooldown that an earlier review started on the critical rule must not
// silence the critical version of a review that was already notified.
func TestCooldownDoesNotSilenceAnEscalation(t *testing.T) {
	cfg := escalationConfig(t)
	cfg.Rules[0].Cooldown = config.Duration(5 * time.Minute)
	n, hass := newTestNotifier(t, cfg)
	ctx := context.Background()

	// An earlier review fires the critical rule directly, starting its cooldown.
	earlier := loadReview(t, "review-new.json")
	earlier.After.ID = "an-earlier-review"
	earlier.After.Data.Zones = []string{"back_porch"}
	n.HandleReview(ctx, earlier)
	before := hass.count()

	escalate(t, n) // the review under test: quiet first, then critical

	critical := 0
	for _, s := range hass.all()[before:] {
		if isCritical(s) {
			critical++
		}
	}
	if critical != 1 {
		t.Errorf("%d critical pushes for the escalated review, want 1", critical)
	}
}

// slowClipProber is Frigate still producing a clip: that probe never returns
// on its own, the others answer at once.
type slowClipProber struct {
	mu     sync.Mutex
	probed []media.Kind
}

func (p *slowClipProber) Check(ctx context.Context, kind media.Kind, _ string, _ int64) error {
	p.mu.Lock()
	p.probed = append(p.probed, kind)
	p.mu.Unlock()
	if kind == media.KindClip {
		<-ctx.Done()
	}
	return ctx.Err()
}

// A clip that is slow to appear must not use the whole probe budget: the end
// update would go out as text and strip the picture the notification had.
func TestSlowClipProbeStillLeavesTimeForTheStill(t *testing.T) {
	old := clipProbeTimeout
	clipProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { clipProbeTimeout = old })

	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	n, hass := newTestNotifier(t, cfg)
	prober := &slowClipProber{}
	n.prober = prober
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, loadReview(t, "review-end.json"))

	data := hass.last(t).payload["data"].(map[string]any)
	if data["image"] == nil {
		t.Errorf("the end update has no image (probed %v): a slow clip starved the still", prober.probed)
	}
}
