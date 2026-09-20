package notifier

import (
	"context"
	"strings"
	"testing"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
)

// The GenAI summary edits the delivered notification quietly, same tag.
func TestGenAISummaryEditsTheNotificationQuietly(t *testing.T) {
	n, hass, chat := newChatNotifier(t, multiTargetConfig(t))
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, loadReview(t, "review-end.json"))
	n.HandleReview(ctx, loadReview(t, "review-genai.json"))

	calls := chat.all()
	if len(calls) != 3 {
		t.Fatalf("chat received %d messages, want new, end and genai", len(calls))
	}
	if calls[0].msg.Update {
		t.Error("the first message is a new alert, not an update")
	}
	for i, c := range calls[1:] {
		if !c.msg.Update || c.prev == "" {
			t.Errorf("message %d: update = %v, prev = %q, want a quiet edit of the first", i+1, c.msg.Update, c.prev)
		}
	}

	last := calls[2].msg
	if last.Headline != "Needs review: Person approaches the garage door" || last.Threat != 1 {
		t.Errorf("headline/threat = %q/%d, want the GenAI summary", last.Headline, last.Threat)
	}
	if last.Stage != "ended" || last.Tag != calls[0].msg.Tag {
		t.Errorf("stage/tag = %q/%q, want the ended review under its original tag", last.Stage, last.Tag)
	}

	if hass.count() != 3 {
		t.Fatalf("hass received %d notifications, want 3", hass.count())
	}
	data := hass.last(t).payload["data"].(map[string]any)
	if msg := hass.last(t).payload["message"].(string); !strings.Contains(msg, "Person approaches the garage door") {
		t.Errorf("hass message = %q, want the GenAI title", msg)
	}
	if data["tag"] != hass.all()[0].payload["data"].(map[string]any)["tag"] {
		t.Error("the GenAI update must replace the notification, so it keeps the tag")
	}
}

// An update never creates a first message.
func TestGenAISummaryNeverCreatesANotification(t *testing.T) {
	n, hass, chat := newChatNotifier(t, multiTargetConfig(t))

	n.HandleReview(context.Background(), loadReview(t, "review-genai.json"))

	if hass.count() != 0 || len(chat.all()) != 0 {
		t.Errorf("sent %d/%d notifications for a review that was never notified", hass.count(), len(chat.all()))
	}
}

// The redelivery guard tells genai from end, drops repeats, takes regenerations.
func TestGenAIRedeliveryAndRegeneration(t *testing.T) {
	n, _, chat := newChatNotifier(t, multiTargetConfig(t))
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, loadReview(t, "review-end.json"))
	genai := loadReview(t, "review-genai.json")
	n.HandleReview(ctx, genai)
	n.HandleReview(ctx, genai)
	if got := len(chat.all()); got != 3 {
		t.Fatalf("chat received %d messages, want 3 (the repeat dropped)", got)
	}

	genai.After.Data.Metadata = []byte(`{"title":"Someone leaves a parcel","shortSummary":"A courier leaves a parcel.","potential_threat_level":0}`)
	n.HandleReview(ctx, genai)
	calls := chat.all()
	if len(calls) != 4 || calls[3].msg.Headline != "Someone leaves a parcel" {
		t.Errorf("a regenerated summary should update again, got %d messages", len(calls))
	}
}

// An escalation is new, under a new tag: same-tag replaces are silent.
func TestEscalationIsANewNotificationUnderANewTag(t *testing.T) {
	cfg := multiTargetConfig(t)
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
	n, hass, chat := newChatNotifier(t, cfg)
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	update := loadReview(t, "review-new.json")
	update.Type = frigate.LifecycleUpdate
	update.After.Data.Zones = []string{"back_porch"}
	n.HandleReview(ctx, update)

	calls := chat.all()
	if len(calls) != 2 {
		t.Fatalf("chat received %d messages, want the original and the escalation", len(calls))
	}
	first, escalated := calls[0].msg, calls[1]
	if escalated.prev != "" || escalated.msg.Update || !escalated.msg.Critical {
		t.Errorf("escalation: prev = %q, update = %v, critical = %v; want a fresh, critical, non-update message",
			escalated.prev, escalated.msg.Update, escalated.msg.Critical)
	}
	if escalated.msg.Tag == first.Tag || !strings.HasSuffix(escalated.msg.Tag, "-esc") {
		t.Errorf("tags = %q then %q, want the escalation under a new one", first.Tag, escalated.msg.Tag)
	}
	if hass.count() != 2 {
		t.Fatalf("hass received %d notifications, want 2", hass.count())
	}
	if hass.all()[0].payload["data"].(map[string]any)["tag"] == hass.all()[1].payload["data"].(map[string]any)["tag"] {
		t.Error("hass escalation must not reuse the quiet notification's tag")
	}

	// The end of the review then refreshes the escalated notification.
	n.HandleReview(ctx, loadReview(t, "review-end.json"))
	calls = chat.all()
	last := calls[len(calls)-1]
	if last.msg.Tag != escalated.msg.Tag || last.prev == "" {
		t.Errorf("end update: tag = %q, prev = %q, want an edit of the escalated message", last.msg.Tag, last.prev)
	}
}

// The audit log must tell a GenAI update from the end update before it.
func TestGenAIUpdateIsAuditedAsGenAI(t *testing.T) {
	events := &fakeEventStore{}
	n, _, _ := newChatNotifier(t, multiTargetConfig(t), WithEventStore(events))
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, loadReview(t, "review-end.json"))
	n.HandleReview(ctx, loadReview(t, "review-genai.json"))

	var phases []string
	for _, rec := range events.notifications {
		if rec.Backend == "slack" {
			phases = append(phases, rec.Phase)
		}
	}
	if want := "new end genai"; strings.Join(phases, " ") != want {
		t.Errorf("slack phases = %v, want %s", phases, want)
	}
}
