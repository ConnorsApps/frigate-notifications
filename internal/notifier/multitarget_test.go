package notifier

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/rules"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
)

// fakeChat stands in for an id-editing backend. A Send with a ref edits and
// keeps it, unless reissue is set (an edit that re-posted).
type fakeChat struct {
	mu      sync.Mutex
	calls   []chatCall
	next    int
	fail    bool
	reissue bool
	// block, when set, holds every Send until it is closed or the send's own
	// context ends, like a hung API.
	block chan struct{}
}

type chatCall struct {
	target config.Target
	msg    sender.Message
	prev   string
}

func (f *fakeChat) Send(ctx context.Context, t config.Target, m sender.Message, prev string) (string, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", errors.New("chat down")
	}
	f.calls = append(f.calls, chatCall{target: t, msg: m, prev: prev})
	if prev != "" && !f.reissue {
		return prev, nil
	}
	f.next++
	return fmt.Sprintf("C1:%d", f.next), nil
}

func (f *fakeChat) all() []chatCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chatCall(nil), f.calls...)
}

// multiTargetConfig: alice, on hass and chat, is the rule's only recipient.
func multiTargetConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Rules[0].To = []string{"alice"}
	cfg.Recipients["alice"] = config.Recipient{
		Targets: []config.Target{
			{Type: config.TargetHass, Service: "mobile_app_alice"},
			{Type: config.TargetSlack, Channel: "C1"},
		},
		AllowCritical: true,
	}
	return cfg
}

func newChatNotifier(t *testing.T, cfg *config.Config, opts ...Option) (*Notifier, *fakeHass, *fakeChat) {
	t.Helper()
	n, hass := newTestNotifier(t, cfg)
	for _, o := range opts {
		o(n)
	}
	chat := &fakeChat{}
	n.senders[config.TargetSlack] = chat
	return n, hass, chat
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Policy runs once per recipient; every target then gets the notification.
func TestEveryTargetOfARecipientReceives(t *testing.T) {
	events := &fakeEventStore{}
	n, hass, chat := newChatNotifier(t, multiTargetConfig(t), WithEventStore(events))

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	if hass.count() != 1 {
		t.Errorf("hass received %d notifications, want 1", hass.count())
	}
	calls := chat.all()
	if len(calls) != 1 {
		t.Fatalf("chat received %d notifications, want 1", len(calls))
	}
	if calls[0].target.Channel != "C1" {
		t.Errorf("chat target = %+v, want channel C1", calls[0].target)
	}
	if calls[0].msg.Body != hass.last(t).payload["message"] {
		t.Error("every target should receive the same message")
	}

	backends := map[string]string{}
	for _, rec := range events.notifications {
		backends[rec.Backend] = rec.Target
		if !rec.Success {
			t.Errorf("record for %s marked failed", rec.Backend)
		}
	}
	if backends["hass"] != "hass:mobile_app_alice" || backends["slack"] != "slack:C1" {
		t.Errorf("audit records = %v, want one per target, keyed by backend", backends)
	}
}

// A failing target affects only itself, and only successes get updates.
func TestFailingTargetDoesNotBlockOthers(t *testing.T) {
	n, hass, chat := newChatNotifier(t, multiTargetConfig(t))
	chat.fail = true

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	if hass.count() != 1 {
		t.Fatalf("hass received %d notifications, want 1 despite the chat failure", hass.count())
	}
	state, ok := n.store.LoadReview(context.Background(), loadReview(t, "review-new.json").After.ID)
	if !ok {
		t.Fatal("review state should be saved for the target that succeeded")
	}
	if len(state.Deliveries) != 1 || state.Deliveries[0].Target != 0 {
		t.Errorf("deliveries = %+v, want only the hass target", state.Deliveries)
	}
}

// A hung backend must not hold up a target that answers.
func TestSlowTargetDoesNotDelayOthers(t *testing.T) {
	cfg := multiTargetConfig(t)
	// The hung target goes first, so a serial loop would stall on it before
	// ever reaching Home Assistant.
	alice := cfg.Recipients["alice"]
	alice.Targets = []config.Target{alice.Targets[1], alice.Targets[0]}
	cfg.Recipients["alice"] = alice
	n, hass, chat := newChatNotifier(t, cfg)
	chat.block = make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	}()

	waitFor(t, "the hass push while chat is still hung", func() bool { return hass.count() == 1 })
	select {
	case <-done:
		t.Fatal("HandleReview returned while the chat send was still blocked")
	default:
	}

	close(chat.block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleReview did not finish after the chat send was released")
	}
}

// Nor may one person's hung backend delay another's push.
func TestSlowRecipientDoesNotDelayAnother(t *testing.T) {
	cfg := multiTargetConfig(t)
	cfg.Rules[0].To = []string{"alice", "bob"}
	n, hass, chat := newChatNotifier(t, cfg)
	chat.block = make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.HandleReview(context.Background(), loadReview(t, "review-new.json"))
	}()

	// bob only has Home Assistant, and alice's hass push is independent of
	// her hung chat target: both land while the chat send is blocked.
	waitFor(t, "both hass pushes", func() bool { return hass.count() == 2 })
	close(chat.block)
	<-done
}

// Each target's ref survives from the first send to the update.
func TestRefsPersistAndAreReusedOnUpdate(t *testing.T) {
	n, _, chat := newChatNotifier(t, multiTargetConfig(t))
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	n.HandleReview(ctx, loadReview(t, "review-end.json"))

	calls := chat.all()
	if len(calls) != 2 {
		t.Fatalf("chat received %d sends, want the original and one update", len(calls))
	}
	if calls[0].prev != "" {
		t.Errorf("first send carried prev %q, want none", calls[0].prev)
	}
	if calls[1].prev != "C1:1" {
		t.Errorf("update carried prev %q, want the ref C1:1 from the first send", calls[1].prev)
	}
	if calls[1].msg.Tag != calls[0].msg.Tag {
		t.Errorf("update tag = %q, want %q", calls[1].msg.Tag, calls[0].msg.Tag)
	}
}

// After an edit re-posts, the next phase edits the new message.
func TestChangedRefIsSaved(t *testing.T) {
	n, _, chat := newChatNotifier(t, multiTargetConfig(t))
	ctx := context.Background()

	first := loadReview(t, "review-new.json")
	n.HandleReview(ctx, first)
	chat.reissue = true
	n.HandleReview(ctx, loadReview(t, "review-end.json"))

	state, ok := n.store.LoadReview(ctx, first.After.ID)
	if !ok {
		t.Fatal("review state missing")
	}
	var slackRef string
	for _, d := range state.Deliveries {
		if d.Target == 1 {
			slackRef = d.Ref
		}
	}
	if slackRef != "C1:2" {
		t.Errorf("saved slack ref = %q, want the reissued C1:2", slackRef)
	}
}

// Updates reach only targets that got the original.
func TestUpdateSkipsTargetsThatNeverReceivedIt(t *testing.T) {
	n, hass, chat := newChatNotifier(t, multiTargetConfig(t))
	chat.fail = true
	ctx := context.Background()

	n.HandleReview(ctx, loadReview(t, "review-new.json"))
	chat.fail = false
	n.HandleReview(ctx, loadReview(t, "review-end.json"))

	if got := len(chat.all()); got != 0 {
		t.Errorf("chat received %d sends, want none: it never got the original", got)
	}
	if hass.count() != 2 {
		t.Errorf("hass received %d, want the original and the update", hass.count())
	}
}

// Rate caps are per person: capped once, digest to every target.
func TestPolicyAppliesOncePerRecipientAndDigestFansOut(t *testing.T) {
	cfg := multiTargetConfig(t)
	alice := cfg.Recipients["alice"]
	alice.MaxPerHour = 2
	cfg.Recipients["alice"] = alice
	n, hass, chat := newChatNotifier(t, cfg)

	for i := 0; i < 6; i++ {
		event := loadReview(t, "review-new.json")
		event.After.ID = fmt.Sprintf("review-%c", 'a'+i)
		n.HandleReview(context.Background(), event)
	}

	isDigest := func(tag string) bool { return strings.HasPrefix(tag, "fn-digest-") }
	var hassNormal, hassDigest, chatNormal, chatDigest int
	for _, s := range hass.all() {
		if isDigest(s.payload["data"].(map[string]any)["tag"].(string)) {
			hassDigest++
		} else {
			hassNormal++
		}
	}
	for _, c := range chat.all() {
		if isDigest(c.msg.Tag) {
			chatDigest++
		} else {
			chatNormal++
		}
	}

	if hassNormal != chatNormal {
		t.Errorf("hass got %d and chat %d normal notifications, want the same: the cap is per recipient", hassNormal, chatNormal)
	}
	if hassDigest != 1 || chatDigest != 1 {
		t.Errorf("digests: hass %d, chat %d, want one on each target", hassDigest, chatDigest)
	}
}

func TestDryRunSendsToNoTarget(t *testing.T) {
	cfg := multiTargetConfig(t)
	cfg.DryRun = true
	n, hass, chat := newChatNotifier(t, cfg)

	n.HandleReview(context.Background(), loadReview(t, "review-new.json"))

	if hass.count() != 0 || len(chat.all()) != 0 {
		t.Error("dry run must not call any backend")
	}
}

// Clip and still resolve independently: chat backends and Android need the
// still.
func TestStillIsKeptAlongsideTheClip(t *testing.T) {
	tests := []struct {
		name      string
		errs      map[media.Kind]error
		wantVideo bool
		wantImage string
		wantAsked []media.Kind
	}{
		{
			name:      "clip and still both resolved",
			wantVideo: true, wantImage: "/m/preview/",
			wantAsked: []media.Kind{media.KindClip, media.KindPreview},
		},
		{
			name:      "still falls back to the snapshot",
			errs:      map[media.Kind]error{media.KindPreview: errors.New("gone")},
			wantVideo: true, wantImage: "/m/snapshot/",
			wantAsked: []media.Kind{media.KindClip, media.KindPreview, media.KindSnapshot},
		},
		{
			name:      "an oversized clip is still linkable and the still is used",
			errs:      map[media.Kind]error{media.KindClip: media.ErrTooLarge},
			wantImage: "/m/preview/",
			wantAsked: []media.Kind{media.KindClip, media.KindPreview},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := multiTargetConfig(t)
			n, _, _ := newChatNotifier(t, cfg)
			prober := &fakeProber{errs: tc.errs}
			n.prober = prober

			review := loadReview(t, "review-end.json").After
			c := n.buildContent(context.Background(), review, cfg.Rules[0], rules.PhaseEnd, "tag", review.PrimaryEventID(), "")

			if (c.Video != "") != tc.wantVideo {
				t.Errorf("video = %q, want present=%v", c.Video, tc.wantVideo)
			}
			if tc.wantImage == "" && c.Image != "" {
				t.Errorf("image = %q, want none", c.Image)
			}
			if tc.wantImage != "" && !strings.Contains(c.Image, tc.wantImage) {
				t.Errorf("image = %q, want it to contain %q", c.Image, tc.wantImage)
			}
			if c.ClipURL == "" {
				t.Error("the clip should stay linkable")
			}
			if !slices.Equal(prober.asked, tc.wantAsked) {
				t.Errorf("probed %v, want %v", prober.asked, tc.wantAsked)
			}
		})
	}
}
