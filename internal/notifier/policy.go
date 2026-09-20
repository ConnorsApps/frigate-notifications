package notifier

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
)

// digestInterval bounds how often a rate-capped recipient gets the rolling
// summary. Without it the digest would itself be one push per event — the
// thing the cap exists to stop.
const digestInterval = 10 * time.Minute

// verdict is the per-recipient decision, after the rule has already matched.
type verdict struct {
	send     bool
	critical bool
	reason   string
}

// applyPolicy applies a recipient's own preferences to a notification the
// rule already selected. Keeping this per-recipient rather than per-rule is
// what removes the need for a separate rule per person — the duplication that
// let the old Home Assistant automations drift apart.
func (n *Notifier) applyPolicy(
	ctx context.Context,
	name string,
	recipient config.Recipient,
	rule config.Rule,
	camera string,
) verdict {
	critical := rule.Critical && recipient.AllowCritical
	now := n.now().In(n.cfg.Location)

	// A critical alert ignores active hours and rate caps by definition: it is
	// the notification the recipient explicitly asked to be woken for.
	if critical {
		return verdict{send: true, critical: true}
	}

	if recipient.ActiveHours != nil && !n.inWindow(ctx, *recipient.ActiveHours, now) {
		return verdict{reason: "outside_active_hours"}
	}

	if recipient.MaxPerHour > 0 {
		count := n.store.CountForHour(ctx, name, now)
		if count > int64(recipient.MaxPerHour) {
			n.sendDigest(ctx, name, recipient, camera, now)
			return verdict{reason: "rate_cap"}
		}
	}

	return verdict{send: true, critical: false}
}

// inWindow evaluates a recipient window, resolving solar endpoints if needed.
//
// Fails open in the direction of sending: an unreadable sun.sun makes the
// window read as open, so a Home Assistant problem can never mute a phone.
func (n *Notifier) inWindow(ctx context.Context, w config.Window, now time.Time) bool {
	var st config.SolarTimes
	if w.NeedsSolar() {
		resolved, err := n.states.Solar(ctx)
		if err != nil {
			n.logger.Warn().Err(err).Msg("solar times unavailable; treating window as open")
			return true
		}
		st = resolved
	}
	return w.Contains(now, st)
}

// sendDigest collapses a rate-capped recipient's overflow into one summary.
func (n *Notifier) sendDigest(
	ctx context.Context,
	name string,
	recipient config.Recipient,
	camera string,
	now time.Time,
) {
	entry := n.store.AddToDigest(ctx, name, camera, now)

	if !n.store.AcquireCooldown(ctx, "digest:"+name+":"+camera, digestInterval) {
		return
	}

	since := time.Unix(entry.SinceTS, 0).In(n.cfg.Location).Format("15:04")
	// Cleared now, not after a successful send: the next digest should count
	// from here either way, and a backend failure must not make the following
	// digest double-count what this one already summarized.
	n.store.ClearDigest(ctx, name, camera)
	noun := "events"
	if entry.Count == 1 {
		noun = "event"
	}
	summary := fmt.Sprintf("%d more %s since %s", entry.Count, noun, since)
	msg := sender.Message{
		Camera:   camera,
		Title:    n.cfg.CameraName(camera),
		Headline: summary,
		Body:     summary,
		Stage:    sender.StageDigest,
		Tag:      "fn-digest-" + camera,
		ClickURL: n.cfg.DashboardURL,
	}

	// Tag-replacing backends keep one summary; id-editing ones post fresh,
	// at most one per digestInterval.
	var wg sync.WaitGroup
	for _, t := range recipient.Targets {
		wg.Go(func() {
			if _, err := n.sendOne(ctx, name, t, msg, ""); err != nil {
				n.logger.Error().Err(err).Str("recipient", name).Str("target", t.String()).
					Msg("failed to send digest notification")
				n.metrics.NotifyError(name, string(t.Type))
			}
		})
	}
	wg.Wait()
}
