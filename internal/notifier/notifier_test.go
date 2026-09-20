package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/eventstore"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/ConnorsApps/frigate-notifications/internal/hasscache"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/rules"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
	"github.com/ConnorsApps/frigate-notifications/internal/store"
)

// fakeEventStore records every audit write instead of persisting it.
type fakeEventStore struct {
	mu            sync.Mutex
	reviews       []frigate.ReviewPayload
	descriptions  []frigate.TrackedObjectUpdate
	notifications []eventstore.NotificationRecord
}

func (f *fakeEventStore) SaveReviewEvent(_ context.Context, review frigate.ReviewPayload, _ frigate.LifecycleType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews = append(f.reviews, review)
}

func (f *fakeEventStore) SaveDescriptionUpdate(_ context.Context, upd frigate.TrackedObjectUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.descriptions = append(f.descriptions, upd)
}

func (f *fakeEventStore) SaveNotification(_ context.Context, rec eventstore.NotificationRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifications = append(f.notifications, rec)
}

// fakeHass records every notification instead of sending it.
type fakeHass struct {
	mu   sync.Mutex
	sent []sentNotification
	fail bool
}

type sentNotification struct {
	service string
	payload map[string]any
}

func (f *fakeHass) Notify(_ context.Context, service string, data map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("hass down")
	}
	f.sent = append(f.sent, sentNotification{service: service, payload: data})
	return nil
}

func (f *fakeHass) all() []sentNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentNotification(nil), f.sent...)
}

func (f *fakeHass) count() int { return len(f.all()) }

func (f *fakeHass) last(t *testing.T) sentNotification {
	t.Helper()
	all := f.all()
	if len(all) == 0 {
		t.Fatal("no notification was sent")
	}
	return all[len(all)-1]
}

// offlineStates makes every Home Assistant lookup fail, exercising the
// fail-open paths.
type offlineStates struct{}

func (offlineStates) EntityState(context.Context, string) ([]byte, error) {
	return nil, errors.New("offline")
}

// hassTargets is a recipient's single Home Assistant target.
func hassTargets(service string) []config.Target {
	return []config.Target{{Type: config.TargetHass, Service: service}}
}

func endpoint(t *testing.T, s string) config.Endpoint {
	t.Helper()
	e, err := config.ParseEndpoint(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	noHoldoff := config.Duration(0)
	return &config.Config{
		Timezone:      "UTC",
		Location:      time.UTC,
		DashboardPath: "/frigate/dashboard",
		Recipients: map[string]config.Recipient{
			"alice": {Targets: hassTargets("mobile_app_alice"), AllowCritical: true},
			"bob":   {Targets: hassTargets("mobile_app_bob"), AllowCritical: true},
		},
		Cameras: map[string]config.Camera{
			"garage":      {FriendlyName: "Garage", LiveViewEntity: "camera.garage"},
			"front_porch": {FriendlyName: "Front Porch", LiveViewEntity: "camera.front_porch"},
		},
		Rules: []config.Rule{{
			Name: "day",
			When: config.RuleConditions{Labels: []string{"person"}},
			// Opt out of the person-matching holdoff default; not under test here.
			Holdoff:       &noHoldoff,
			To:            []string{"alice", "bob"},
			Preset:        config.PresetAuto,
			CooldownScope: config.ScopeCamera,
		}},
	}
}

func newTestNotifier(t *testing.T, cfg *config.Config) (*Notifier, *fakeHass) {
	t.Helper()
	hass := &fakeHass{}
	signer, err := media.NewSigner(strings.Repeat("ab", 16), "https://media.test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	n := New(cfg, sender.New(cfg, hass), hasscache.New(offlineStates{}, cfg.Location),
		store.New(context.Background(), "", nil), signer)
	return n, hass
}

func loadReview(t *testing.T, name string) frigate.ReviewEvent {
	t.Helper()
	data, err := os.ReadFile("../frigate/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var event frigate.ReviewEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestNotifiesEveryRecipient(t *testing.T) {
	cfg := testConfig(t)
	n, hass := newTestNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	sent := hass.all()
	if len(sent) != 2 {
		t.Fatalf("sent %d notifications, want one per recipient", len(sent))
	}
	services := map[string]bool{sent[0].service: true, sent[1].service: true}
	if !services["mobile_app_alice"] || !services["mobile_app_bob"] {
		t.Errorf("services = %v, want both recipients", services)
	}
	// One rule fans out to everyone, so the two payloads are identical —
	// this is what replaces the per-person automation that drifted.
	if sent[0].payload["message"] != sent[1].payload["message"] {
		t.Error("both recipients should receive the same message")
	}
}

func TestIgnoresUnconfiguredCamera(t *testing.T) {
	cfg := testConfig(t)
	delete(cfg.Cameras, "garage")
	n, hass := newTestNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if hass.count() != 0 {
		t.Error("a review from an unconfigured camera should be ignored")
	}
}

// Identical bytes delivered twice (QoS 1) must not produce a second push.
// Content-changing republishes are a different case, covered below.
func TestSuppressesRedelivery(t *testing.T) {
	n, hass := newTestNotifier(t, testConfig(t))
	event := loadReview(t, "review-new.json")

	n.HandleReview(context.Background(), event)
	n.HandleReview(context.Background(), event)

	if got := hass.count(); got != 2 {
		t.Errorf("sent %d notifications, want 2 (the redelivered copy suppressed)", got)
	}
}

func TestCooldownBlocksASecondReview(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].Cooldown = config.Duration(time.Minute)
	n, hass := newTestNotifier(t, cfg)

	first := loadReview(t, "review-new.json")
	n.HandleReview(context.Background(), first)

	second := loadReview(t, "review-new.json")
	second.After.ID = "a-different-review"
	n.HandleReview(context.Background(), second)

	if got := hass.count(); got != 2 {
		t.Errorf("sent %d notifications, want only the first review's 2", got)
	}
}

// Frigate publishes an "update" on every change to a review, not once. The
// redelivery guard therefore has to key on the payload's content: keying on
// (review, phase) alone dropped every update after the first, so a
// detection-to-alert escalation that didn't land in the first one was lost.
func TestEscalationInALaterUpdateIsNotDroppedAsARedelivery(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules = []config.Rule{{
		Name:   "alerts-only",
		When:   config.RuleConditions{Severity: []string{"alert"}},
		To:     []string{"alice"},
		Preset: config.PresetAuto,
	}}
	n, hass := newTestNotifier(t, cfg)

	event := loadReview(t, "review-new.json")
	event.After.Severity = frigate.SeverityDetection
	n.HandleReview(context.Background(), event)
	if hass.count() != 0 {
		t.Fatal("a detection should not match an alert-only rule")
	}

	// An update carrying a new zone, still a detection.
	event.Type = frigate.LifecycleUpdate
	event.After.Data.Zones = append(event.After.Data.Zones, "driveway")
	n.HandleReview(context.Background(), event)

	// A later update, now escalated to an alert.
	event.After.Severity = frigate.SeverityAlert
	n.HandleReview(context.Background(), event)

	if hass.count() != 1 {
		t.Errorf("sent %d notifications, want 1 from the escalation in the second update", hass.count())
	}
}

// The end phase refreshes the notification already on the phone rather than
// sending a second one.
func TestEndPhaseUpdatesInPlace(t *testing.T) {
	n, hass := newTestNotifier(t, testConfig(t))

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	first := hass.last(t)

	n.HandleReview(context.Background(), loadReview(t, "review-end.json"))
	last := hass.last(t)

	if hass.count() != 4 {
		t.Fatalf("sent %d notifications, want 2 initial + 2 updates", hass.count())
	}

	firstTag := first.payload["data"].(map[string]any)["tag"]
	lastTag := last.payload["data"].(map[string]any)["tag"]
	if firstTag != lastTag {
		t.Errorf("update tag = %v, want the original %v so it replaces in place", lastTag, firstTag)
	}

	// The clip only exists once the review has ended.
	if _, ok := first.payload["data"].(map[string]any)["attachment"]; ok {
		t.Error("the initial notification should not carry a clip")
	}
	if _, ok := last.payload["data"].(map[string]any)["attachment"]; !ok {
		t.Error("the end update should carry the clip")
	}
}

// Rule order is priority, and an end-phase re-match may only move upward.
// Without this, a late match on a quieter rule silently demotes a critical
// alert that already woke someone up.
func TestEndPhaseNeverDowngrades(t *testing.T) {
	cfg := testConfig(t)
	zeroHoldoff := config.Duration(0)
	cfg.Rules = []config.Rule{
		{
			Name:     "critical-person",
			When:     config.RuleConditions{Labels: []string{"person"}},
			Holdoff:  &zeroHoldoff,
			To:       []string{"alice"},
			Preset:   config.PresetAuto,
			Critical: true,
		},
		{
			Name:   "quiet-catchall",
			When:   config.RuleConditions{},
			To:     []string{"alice"},
			Preset: config.PresetText,
		},
	}
	n, chat := withChat(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if calls := chat.all(); len(calls) != 1 || !calls[0].msg.Critical {
		t.Fatal("the first notification should be critical")
	}

	// At end the object list is gone, so only the catchall matches — but the
	// notification must stay critical.
	end := loadReview(t, "review-end.json")
	end.After.Data.Objects = nil
	n.HandleReview(context.Background(), end)

	calls := chat.all()
	if len(calls) != 2 || !calls[1].msg.Critical || !calls[1].msg.Update {
		t.Errorf("end update = %+v, want it to stay critical and edit in place", calls)
	}
}

// The mirror image: a review that starts on a low-priority rule and later
// matches a higher-priority one must escalate with a fresh notification.
func TestEscalatesToHigherPriorityRule(t *testing.T) {
	cfg := testConfig(t)
	zeroHoldoff := config.Duration(0)
	cfg.Rules = []config.Rule{
		{
			Name:     "person-in-zone",
			When:     config.RuleConditions{Zones: []string{"back_porch"}},
			To:       []string{"alice"},
			Preset:   config.PresetAuto,
			Critical: true,
		},
		{
			Name:    "any-person",
			When:    config.RuleConditions{Labels: []string{"person"}},
			Holdoff: &zeroHoldoff,
			To:      []string{"alice"},
			Preset:  config.PresetAuto,
		},
	}
	n, hass := newTestNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if isCritical(hass.last(t)) {
		t.Fatal("the initial notification should match the lower-priority rule")
	}

	// The person has now entered the zone.
	update := loadReview(t, "review-new.json")
	update.Type = frigate.LifecycleUpdate
	update.After.Data.Zones = []string{"back_porch"}
	n.HandleReview(context.Background(), update)

	last := hass.last(t)
	if !isCritical(last) {
		t.Error("entering the zone should escalate to the critical rule")
	}
	if !strings.Contains(last.payload["message"].(string), "Back Porch") {
		t.Errorf("message = %q, want it to name the zone", last.payload["message"])
	}
}

// A higher-priority rule that matches once a zone populates but does not
// raise the notification to critical should edit the existing push in place
// (same tag, still quiet), not send a second one.
func TestHigherPriorityNonCriticalRuleRefreshesInPlace(t *testing.T) {
	cfg := testConfig(t)
	zeroHoldoff := config.Duration(0)
	cfg.Rules = []config.Rule{
		{
			Name:   "zone-loiter",
			When:   config.RuleConditions{Zones: []string{"back_porch"}},
			To:     []string{"alice"},
			Preset: config.PresetAuto,
		},
		{
			Name:    "any-person",
			When:    config.RuleConditions{Labels: []string{"person"}},
			Holdoff: &zeroHoldoff,
			To:      []string{"alice"},
			Preset:  config.PresetAuto,
		},
	}
	n, hass := newTestNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if hass.count() != 1 {
		t.Fatalf("sent %d notifications, want 1 initial push under any-person", hass.count())
	}
	first := hass.last(t)

	update := loadReview(t, "review-new.json")
	update.Type = frigate.LifecycleUpdate
	update.After.Data.Zones = []string{"back_porch"}
	n.HandleReview(context.Background(), update)

	if hass.count() != 2 {
		t.Fatalf("sent %d notifications, want the higher-priority match to edit in place (one more deliver, no fresh fan-out)", hass.count())
	}
	last := hass.last(t)

	firstTag := first.payload["data"].(map[string]any)["tag"]
	lastTag := last.payload["data"].(map[string]any)["tag"]
	if firstTag != lastTag {
		t.Errorf("refresh tag = %v, want the original %v so it replaces in place", lastTag, firstTag)
	}
	if isCritical(last) {
		t.Error("a non-critical higher-priority rule must not turn the notification critical")
	}
	if !strings.Contains(last.payload["message"].(string), "Back Porch") {
		t.Errorf("message = %q, want the refresh to re-render under the zone rule", last.payload["message"])
	}
}

// A higher-priority rule that is critical, superseding a rule that was
// already critical, is not a fresh escalation (prior.Critical is already
// true) — it must still refresh in place, and the refresh must stay
// critical rather than silently downgrading to a quiet push.
func TestHigherPriorityCriticalRuleStaysCriticalOnRefresh(t *testing.T) {
	cfg := testConfig(t)
	zeroHoldoff := config.Duration(0)
	cfg.Rules = []config.Rule{
		{
			Name:     "zone-critical",
			When:     config.RuleConditions{Zones: []string{"back_porch"}},
			To:       []string{"alice", "bob"},
			Preset:   config.PresetAuto,
			Critical: true,
		},
		{
			Name:     "any-person-critical",
			When:     config.RuleConditions{Labels: []string{"person"}},
			Holdoff:  &zeroHoldoff,
			To:       []string{"alice"},
			Preset:   config.PresetAuto,
			Critical: true,
		},
	}
	n, chat := withChat(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if calls := chat.all(); len(calls) != 1 || !calls[0].msg.Critical {
		t.Fatal("the initial notification should be critical")
	}

	update := loadReview(t, "review-new.json")
	update.Type = frigate.LifecycleUpdate
	update.After.Data.Zones = []string{"back_porch"}
	n.HandleReview(context.Background(), update)

	calls := chat.all()
	if len(calls) != 2 {
		t.Fatalf("sent %d messages, want the higher-priority match to refresh in place (2 total), not fan out again", len(calls))
	}
	if last := calls[1]; last.prev == "" || !last.msg.Update || !last.msg.Critical {
		t.Errorf("refresh = %+v, want an in-place edit that stays critical", last)
	}
}

// isCritical reports whether a Home Assistant payload is a critical alert.
func isCritical(n sentNotification) bool {
	data, ok := n.payload["data"].(map[string]any)
	if !ok {
		return false
	}
	push, ok := data["push"].(map[string]any)
	return ok && push["interruption-level"] == "critical"
}

// withChat adds a Slack target for alice. Chat keeps Critical on updates (hass
// drops it), so it shows an update didn't downgrade a review.
func withChat(t *testing.T, cfg *config.Config) (*Notifier, *fakeChat) {
	t.Helper()
	alice := cfg.Recipients["alice"]
	alice.Targets = append(alice.Targets, config.Target{Type: config.TargetSlack, Channel: "C1"})
	cfg.Recipients["alice"] = alice
	n, _, chat := newChatNotifier(t, cfg)
	return n, chat
}

// A recipient who hasn't opted into critical pushes gets the notification,
// just not the alarm.
func TestCriticalDowngradedPerRecipient(t *testing.T) {
	cfg := testConfig(t)
	cfg.Recipients["bob"] = config.Recipient{Targets: hassTargets("mobile_app_bob"), AllowCritical: false}
	cfg.Rules[0].Critical = true
	n, hass := newTestNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	for _, s := range hass.all() {
		wantCritical := s.service == "mobile_app_alice"
		if isCritical(s) != wantCritical {
			t.Errorf("%s critical = %v, want %v", s.service, isCritical(s), wantCritical)
		}
	}
}

func TestOutsideActiveHoursSuppressesNonCritical(t *testing.T) {
	cfg := testConfig(t)
	awake := config.Window{From: endpoint(t, "09:00"), To: endpoint(t, "21:00")}
	cfg.Recipients["bob"] = config.Recipient{
		Targets:       hassTargets("mobile_app_bob"),
		AllowCritical: true,
		ActiveHours:   &awake,
	}
	cfg.Rules[0].To = []string{"bob"}

	n, hass := newTestNotifier(t, cfg)
	n.now = func() time.Time { return time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC) }

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if hass.count() != 0 {
		t.Error("a non-critical notification at 03:00 is outside 09:00-21:00 and should be suppressed")
	}
}

// Critical is exactly the notification someone asked to be woken for, so it
// ignores the active-hours window by definition.
func TestActiveHoursDoNotSuppressCritical(t *testing.T) {
	cfg := testConfig(t)
	awake := config.Window{From: endpoint(t, "09:00"), To: endpoint(t, "21:00")}
	cfg.Recipients["bob"] = config.Recipient{
		Targets:       hassTargets("mobile_app_bob"),
		AllowCritical: true,
		ActiveHours:   &awake,
	}
	cfg.Rules[0].To = []string{"bob"}
	cfg.Rules[0].Critical = true

	n, hass := newTestNotifier(t, cfg)
	n.now = func() time.Time { return time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC) }

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if hass.count() != 1 {
		t.Error("a critical notification should break through the active-hours window")
	}
}

func TestRateCapCollapsesIntoADigest(t *testing.T) {
	cfg := testConfig(t)
	cfg.Recipients = map[string]config.Recipient{
		"alice": {Targets: hassTargets("mobile_app_alice"), AllowCritical: true, MaxPerHour: 2},
	}
	cfg.Rules[0].To = []string{"alice"}
	n, hass := newTestNotifier(t, cfg)

	for i := 0; i < 6; i++ {
		event := loadReview(t, "review-new.json")
		event.After.ID = "review-" + string(rune('a'+i))
		n.HandleReview(context.Background(), event)
	}

	var normal, digests int
	for _, s := range hass.all() {
		if strings.HasPrefix(s.payload["data"].(map[string]any)["tag"].(string), "fn-digest-") {
			digests++
		} else {
			normal++
		}
	}

	if normal > 3 {
		t.Errorf("sent %d normal notifications, want the cap to hold them near 2", normal)
	}
	if digests == 0 {
		t.Error("suppressed events should surface as a digest rather than vanishing")
	}
	// The digest replaces itself on the phone, so one push covers the run.
	if digests > 1 {
		t.Errorf("sent %d digests, want the digest interval to hold it to 1", digests)
	}
}

// A holdoff exists because face recognition lands after the review opens; a
// push cannot be recalled, so the decision has to wait for it.
func TestHoldoffDelaysTheDecision(t *testing.T) {
	cfg := testConfig(t)
	holdoff := config.Duration(80 * time.Millisecond)
	cfg.Rules[0].Holdoff = &holdoff
	n, hass := newTestNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	if hass.count() != 0 {
		t.Fatal("the decision should not have been made yet")
	}

	time.Sleep(200 * time.Millisecond)
	if hass.count() != 2 {
		t.Errorf("sent %d notifications after the holdoff, want 2", hass.count())
	}
}

// namedUpdate is the update Frigate publishes when a face is recognized.
func namedUpdate(t *testing.T, name string) frigate.ReviewEvent {
	t.Helper()
	event := loadReview(t, "review-new.json")
	event.Type = frigate.LifecycleUpdate
	event.After.Data.SubLabels = []string{name}
	return event
}

// The hold must decide on the face update, not the stale "new".
func TestHoldoffDecidesOnTheLatestPayload(t *testing.T) {
	cfg := testConfig(t)
	holdoff := config.Duration(80 * time.Millisecond)
	cfg.Rules[0].Holdoff = &holdoff
	cfg.Rules[0].When.ExcludeSubLabels = []string{"alice"}
	n, hass := newTestNotifier(t, cfg)
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, namedUpdate(t, "alice"))

	time.Sleep(250 * time.Millisecond)
	if got := hass.count(); got != 0 {
		t.Errorf("sent %d notifications about an excluded person, want none", got)
	}
}

// A matching update waits out the hold, and the push names the person.
func TestHoldoffWaitsForTheFaceUpdate(t *testing.T) {
	cfg := testConfig(t)
	holdoff := config.Duration(80 * time.Millisecond)
	cfg.Rules[0].Holdoff = &holdoff
	n, hass := newTestNotifier(t, cfg)
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, namedUpdate(t, "carol"))
	if got := hass.count(); got != 0 {
		t.Fatalf("sent %d notifications during the hold, want none", got)
	}

	time.Sleep(250 * time.Millisecond)
	if got := hass.count(); got != 2 {
		t.Fatalf("sent %d notifications after the hold, want 2", got)
	}
	if msg := hass.last(t).payload["message"].(string); !strings.Contains(msg, "Carol") {
		t.Errorf("message = %q, want the recognized name", msg)
	}
}

// A review ending during its hold is decided once, on the end payload.
func TestReviewEndingDuringHoldoffIsDecidedOnce(t *testing.T) {
	cfg := testConfig(t)
	holdoff := config.Duration(80 * time.Millisecond)
	cfg.Rules[0].Holdoff = &holdoff
	n, hass := newTestNotifier(t, cfg)
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, loadReview(t, "review-end.json"))
	time.Sleep(250 * time.Millisecond)

	if got := hass.count(); got != 2 {
		t.Errorf("sent %d notifications, want 2 (one decision, two recipients)", got)
	}
}

func TestGenAIDescriptionAppearsInTheUpdate(t *testing.T) {
	n, hass := newTestNotifier(t, testConfig(t))
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))

	// The description arrives on its own topic, after the initial push.
	n.HandleTrackedObjectUpdate(ctx, frigate.TrackedObjectUpdate{
		Type:        frigate.TrackedObjectUpdateDescription,
		ID:          "1787614649.646371-ums8dg",
		Description: "A person walks up the driveway and tries the door handle.",
	})

	n.HandleReview(ctx, loadReview(t, "review-end.json"))

	msg := hass.last(t).payload["message"].(string)
	if !strings.Contains(msg, "tries the door handle") {
		t.Errorf("end message = %q, want the GenAI description folded in", msg)
	}
}

// A "face" update must not be cached as a GenAI description.
func TestFaceUpdateIsIgnored(t *testing.T) {
	n, _ := newTestNotifier(t, testConfig(t))
	ctx := context.Background()

	name := "alice"
	n.HandleTrackedObjectUpdate(ctx, frigate.TrackedObjectUpdate{
		Type:  frigate.TrackedObjectUpdateFace,
		ID:    "1787614649.646371-ums8dg",
		Name:  &name,
		Score: 0.95,
	})

	if desc := n.store.Description(ctx, "1787614649.646371-ums8dg"); desc != "" {
		t.Errorf("a face update should not populate a description, got %q", desc)
	}
}

// The event store is an audit sink, not a decision input: it must record
// every review (even one that doesn't end up notifying) and every delivery
// attempt, without ever being consulted by the notification path itself.
func TestEventStoreRecordsReviewsAndNotifications(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	events := &fakeEventStore{}
	hass := &fakeHass{}
	signer, err := media.NewSigner(strings.Repeat("ab", 16), "https://media.test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	n := New(cfg, sender.New(cfg, hass), hasscache.New(offlineStates{}, cfg.Location),
		store.New(context.Background(), "", nil), signer, WithEventStore(events))
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleTrackedObjectUpdate(ctx, frigate.TrackedObjectUpdate{
		Type:        frigate.TrackedObjectUpdateDescription,
		ID:          "1787614649.646371-ums8dg",
		Description: "A person walks up the driveway.",
	})

	if len(events.reviews) != 1 {
		t.Fatalf("recorded %d review events, want 1", len(events.reviews))
	}
	if len(events.descriptions) != 1 {
		t.Fatalf("recorded %d description updates, want 1", len(events.descriptions))
	}
	if len(events.notifications) != 1 {
		t.Fatalf("recorded %d notification records, want 1", len(events.notifications))
	}
	rec := events.notifications[0]
	if !rec.Success || rec.DryRun {
		t.Errorf("notification record = %+v, want a successful, non-dry-run send", rec)
	}
	if rec.Recipient != "alice" || rec.ReviewID == "" || rec.Message == "" {
		t.Errorf("notification record = %+v, missing recipient/reviewId/message", rec)
	}
}

// A real Frigate event is worth an audit record even when it doesn't match
// any configured camera — only the notification decision is skipped.
func TestEventStoreRecordsUnconfiguredCameraReviews(t *testing.T) {
	cfg := testConfig(t)
	delete(cfg.Cameras, "garage")
	events := &fakeEventStore{}
	hass := &fakeHass{}
	n := New(cfg, sender.New(cfg, hass), hasscache.New(offlineStates{}, cfg.Location),
		store.New(context.Background(), "", nil), nil, WithEventStore(events))

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	if len(events.reviews) != 1 {
		t.Errorf("recorded %d review events, want 1 even for an unconfigured camera", len(events.reviews))
	}
	if hass.count() != 0 {
		t.Error("an unconfigured camera should still not notify")
	}
}

// A failed send is still worth an audit record — that's the point of an
// audit trail — but must be marked as failed, not silently dropped.
func TestEventStoreRecordsFailedSends(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	events := &fakeEventStore{}
	hass := &fakeHass{fail: true}
	n := New(cfg, sender.New(cfg, hass), hasscache.New(offlineStates{}, cfg.Location),
		store.New(context.Background(), "", nil), nil, WithEventStore(events))

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	if len(events.notifications) != 1 {
		t.Fatalf("recorded %d notification records, want 1", len(events.notifications))
	}
	if rec := events.notifications[0]; rec.Success || rec.Error == "" {
		t.Errorf("notification record = %+v, want Success=false and a non-empty Error", rec)
	}
}

func TestPresetPayloads(t *testing.T) {
	tests := []struct {
		preset      config.Preset
		wantKeys    []string
		notWantKeys []string
	}{
		{preset: config.PresetText, notWantKeys: []string{"image", "video", "entity_id"}},
		{preset: config.PresetAuto, wantKeys: []string{"image"}, notWantKeys: []string{"video", "entity_id"}},
		{preset: config.PresetLiveView, wantKeys: []string{"image", "entity_id"}, notWantKeys: []string{"video"}},
	}

	for _, tc := range tests {
		t.Run(string(tc.preset), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Rules[0].Preset = tc.preset
			cfg.Rules[0].To = []string{"alice"}
			n, hass := newTestNotifier(t, cfg)

			n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
			data := hass.last(t).payload["data"].(map[string]any)

			for _, key := range tc.wantKeys {
				if _, ok := data[key]; !ok {
					t.Errorf("preset %s is missing %q", tc.preset, key)
				}
			}
			for _, key := range tc.notWantKeys {
				if _, ok := data[key]; ok {
					t.Errorf("preset %s should not carry %q at the new phase", tc.preset, key)
				}
			}
			if data["tag"] == "" {
				t.Error("every notification needs a tag so later phases can replace it")
			}
		})
	}
}

// Media links must be signed and point at the proxy, never at Frigate: the
// phone can't authenticate to Frigate from outside the network.
func TestMediaLinksArePublicAndSigned(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	n, hass := newTestNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	image := hass.last(t).payload["data"].(map[string]any)["image"].(string)

	if !strings.HasPrefix(image, "https://media.test/m/snapshot/") {
		t.Errorf("image = %q, want a link to the media proxy", image)
	}
	if !strings.Contains(image, "sig=") || !strings.Contains(image, "exp=") {
		t.Errorf("image = %q, want it signed and expiring", image)
	}
	// Media keys off the Frigate event id, not the review id.
	if !strings.Contains(image, "1787614649.646371-ums8dg") {
		t.Errorf("image = %q, want the event id from data.detections", image)
	}
}

// With no signing key the service still notifies; it just goes text-only,
// rather than sending links that would only 403.
func TestDegradesToTextWithoutASigner(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	hass := &fakeHass{}
	n := New(cfg, sender.New(cfg, hass), hasscache.New(offlineStates{}, cfg.Location),
		store.New(context.Background(), "", nil), nil)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	if hass.count() != 1 {
		t.Fatal("a missing media config should not stop notifications")
	}
	if _, ok := hass.last(t).payload["data"].(map[string]any)["image"]; ok {
		t.Error("no media link should be emitted without a signer")
	}
}

func TestHeadline(t *testing.T) {
	tests := []struct {
		name   string
		review frigate.ReviewPayload
		want   string
	}{
		{
			name:   "objects only",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person"}}},
			want:   "Person detected",
		},
		{
			name:   "object in a zone",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person"}, Zones: []string{"back_porch"}}},
			want:   "Person detected in Back Porch",
		},
		{
			name:   "a recognized face wins over the label",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person"}, SubLabels: []string{"alice"}}},
			want:   "Alice detected",
		},
		{
			name:   "a verified object is still a person",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person-verified"}, SubLabels: []string{"alice"}}},
			want:   "Alice detected",
		},
		{
			name:   "a verified object with no name is not shown as verified",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"car-verified"}}},
			want:   "Car detected",
		},
		{
			name:   "a recognized face plus an unrecognized person",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person", "person"}, SubLabels: []string{"alice"}}},
			want:   "Alice and Another Person detected",
		},
		{
			name:   "a recognized face plus two unrecognized people",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person", "person", "person"}, SubLabels: []string{"alice"}}},
			want:   "Alice and 2 More People detected",
		},
		{
			name:   "a recognized face plus a different object",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person", "dog"}, SubLabels: []string{"alice"}}},
			want:   "Alice and Dog detected",
		},
		{
			name:   "two recognized faces",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person", "person"}, SubLabels: []string{"alice", "bob"}}},
			want:   "Alice and Bob detected",
		},
		{
			name:   "several objects",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person", "dog"}}},
			want:   "Person and Dog detected",
		},
		{
			name:   "the same object twice is named once",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person", "person"}}},
			want:   "Person detected",
		},
		{
			name:   "nothing identified",
			review: frigate.ReviewPayload{},
			want:   "Activity detected",
		},
		{
			name:   "sound alone",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Audio: []string{"speech"}}},
			want:   "Speech heard",
		},
		{
			name:   "sound alongside an object is not the headline",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person"}, Audio: []string{"speech"}}},
			want:   "Person detected",
		},
		{
			name:   "a name that starts with a multi-byte letter",
			review: frigate.ReviewPayload{Data: frigate.ReviewData{Objects: []string{"person"}, SubLabels: []string{"élodie"}}},
			want:   "Élodie detected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildHeadline(tc.review); got != tc.want {
				t.Errorf("buildHeadline = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompose(t *testing.T) {
	cfg := testConfig(t)
	start, end := 1787614651.5, 1787614663.5
	review := frigate.ReviewPayload{
		Camera: "garage", Severity: frigate.SeverityAlert, StartTime: start,
		Data: frigate.ReviewData{Objects: []string{"person-verified", "person-verified"}, Zones: []string{"back_porch"}},
	}

	t.Run("facts", func(t *testing.T) {
		msg := compose(cfg, review, rules.PhaseNew, "fn-x", "")
		if msg.Title != "Garage" || msg.Camera != "garage" || msg.Severity != "alert" {
			t.Errorf("title/camera/severity = %q/%q/%q", msg.Title, msg.Camera, msg.Severity)
		}
		if msg.Stage != sender.StageStarted || !msg.End.IsZero() || msg.Start.Unix() != 1787614651 {
			t.Errorf("stage/start/end = %v/%v/%v", msg.Stage, msg.Start, msg.End)
		}
		if !slices.Equal(msg.Objects, []string{"person"}) || !slices.Equal(msg.Zones, []string{"Back Porch"}) {
			t.Errorf("objects/zones = %v/%v", msg.Objects, msg.Zones)
		}
		if msg.Body != "Person detected in Back Porch" || msg.Detail != "" {
			t.Errorf("body/detail = %q/%q", msg.Body, msg.Detail)
		}
	})

	t.Run("ended", func(t *testing.T) {
		ended := review
		ended.EndTime = &end
		msg := compose(cfg, ended, rules.PhaseEnd, "fn-x", "")
		if msg.Stage != sender.StageEnded || msg.End.Unix() != 1787614663 {
			t.Errorf("stage/end = %v/%v", msg.Stage, msg.End)
		}
	})

	t.Run("object description is the detail", func(t *testing.T) {
		msg := compose(cfg, review, rules.PhaseEnd, "fn-x", "Walking toward the door. Then leaves.")
		if msg.Detail != "Walking toward the door." {
			t.Errorf("detail = %q", msg.Detail)
		}
		if msg.Body != "Person detected in Back Porch\nWalking toward the door." {
			t.Errorf("body = %q", msg.Body)
		}
	})

	t.Run("genai summary replaces the headline and description", func(t *testing.T) {
		event := loadReview(t, "review-genai.json")
		msg := compose(cfg, event.After, rules.PhaseEnd, "fn-x", "An object description that loses.")
		if msg.Headline != "Needs review: Person approaches the garage door" {
			t.Errorf("headline = %q", msg.Headline)
		}
		if !strings.HasPrefix(msg.Detail, "A person walks up to the garage door") || msg.Threat != 1 {
			t.Errorf("detail/threat = %q/%d", msg.Detail, msg.Threat)
		}
	})

	t.Run("an overlong GenAI title is bounded", func(t *testing.T) {
		event := loadReview(t, "review-genai.json")
		event.After.Data.Metadata = json.RawMessage(`{"title":"` + strings.Repeat("word ", 100) + `"}`)
		if got := len([]rune(compose(cfg, event.After, rules.PhaseEnd, "fn-x", "").Headline)); got > maxTitle+1 {
			t.Errorf("headline = %d runes, want at most %d", got, maxTitle+1)
		}
	})

	t.Run("a high threat level is named", func(t *testing.T) {
		event := loadReview(t, "review-genai.json")
		event.After.Data.Metadata = json.RawMessage(`{"title":"Person tries the door","shortSummary":"","potential_threat_level":2}`)
		if got := compose(cfg, event.After, rules.PhaseEnd, "fn-x", "").Headline; got != "Security concern: Person tries the door" {
			t.Errorf("headline = %q", got)
		}
	})
}

// An odd GenAI shape degrades to the plain message.
func TestGenAIMetadataIsTolerant(t *testing.T) {
	for name, raw := range map[string]string{
		"absent":         ``,
		"null":           `null`,
		"empty object":   `{}`,
		"wrong type":     `{"title": 5}`,
		"not an object":  `"text"`,
		"nothing usable": `{"scene":"only the long form"}`,
	} {
		t.Run(name, func(t *testing.T) {
			data := frigate.ReviewData{Metadata: json.RawMessage(raw)}
			if m := data.GenAI(); m != nil {
				t.Errorf("GenAI = %+v, want nil", m)
			}
		})
	}
}

func TestCooldownScopeKeys(t *testing.T) {
	rule := config.Rule{Name: "day"}
	tests := []struct {
		scope config.CooldownScope
		want  string
	}{
		{scope: config.ScopeCamera, want: "day:garage"},
		{scope: config.ScopeRule, want: "day"},
		{scope: config.ScopeGlobal, want: "global"},
	}
	for _, tc := range tests {
		rule.CooldownScope = tc.scope
		if got := cooldownKey(rule, 0, "garage"); got != tc.want {
			t.Errorf("scope %s = %q, want %q", tc.scope, got, tc.want)
		}
	}
}

// A cross-camera cooldown is the point of the rule scope: one person walking
// front_porch -> garage should produce one push, not two.
func TestRuleScopedCooldownSpansCameras(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].Cooldown = config.Duration(time.Minute)
	cfg.Rules[0].CooldownScope = config.ScopeRule
	cfg.Rules[0].To = []string{"alice"}
	n, hass := newTestNotifier(t, cfg)

	first := loadReview(t, "review-new.json")
	n.HandleReview(context.Background(), first)

	second := loadReview(t, "review-new.json")
	second.After.ID = "second-review"
	second.After.Camera = "front_porch"
	n.HandleReview(context.Background(), second)

	if hass.count() != 1 {
		t.Errorf("sent %d notifications, want 1 across both cameras", hass.count())
	}
}

// A failed send must not be recorded as delivered, or the end-phase update
// would try to edit a notification that never arrived.
func TestFailedSendIsNotRecorded(t *testing.T) {
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	n, hass := newTestNotifier(t, cfg)
	hass.fail = true

	ctx := context.Background()
	n.HandleReview(ctx, loadReview(t, "review-new.json"))

	if _, ok := n.store.LoadReview(ctx, "1787614651.122803-w0zlfu"); ok {
		t.Error("a review whose notification failed should not be recorded as sent")
	}
}

// A holdoff belongs to the rule that needs it. A rule waiting on face
// recognition must not delay an unrelated rule that already has everything.
func TestHoldoffAppliesOnlyToTheMatchedRule(t *testing.T) {
	cfg := testConfig(t)
	holdoff := config.Duration(2 * time.Second)
	zeroHoldoff := config.Duration(0)
	cfg.Rules = []config.Rule{
		{
			Name:    "faces",
			When:    config.RuleConditions{Cameras: []string{"front_porch"}},
			Holdoff: &holdoff,
			To:      []string{"alice"},
			Preset:  config.PresetAuto,
		},
		{
			Name: "everything-else",
			When: config.RuleConditions{Labels: []string{"person"}},
			// Opt out: this test isolates the other rule's holdoff.
			Holdoff: &zeroHoldoff,
			To:      []string{"alice"},
			Preset:  config.PresetAuto,
		},
	}
	n, hass := newTestNotifier(t, cfg)

	// The review is from the garage, so the front_porch rule can't match and
	// its holdoff must not apply.
	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	if hass.count() != 1 {
		t.Errorf("sent %d notifications immediately, want 1 — an unrelated rule's holdoff should not delay this", hass.count())
	}
}
