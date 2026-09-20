// Package sender delivers one notification to one target on one backend.
// A Message carries neutral facts; each Sender lays them out for its backend.
package sender

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// Stage is where the review was when the message was made.
type Stage string

const (
	StageStarted Stage = "started"
	StageUpdated Stage = "updated"
	StageEnded   Stage = "ended" // a clip exists and the duration is known
	StageDigest  Stage = "digest"
)

// Message is one notification, before a backend lays it out.
type Message struct {
	Camera string
	Title  string // friendly camera name
	// Headline is what happened, or the GenAI title; Detail is GenAI text.
	// Both may be model-written, so escape them for markup. Body is both on
	// one field.
	Headline string
	Detail   string
	Body     string

	Objects  []string // labels without "-verified"
	Zones    []string // display names
	Severity string   // "alert" or "detection"
	Threat   int      // GenAI verdict 0-2; shown, never acted on

	Stage Stage
	Start time.Time
	End   time.Time // zero while ongoing

	Tag string // stable per review; replaces by key where supported

	Image   string // still safe to attach
	Video   string // clip that fits an inline attachment
	ClipURL string // clip link, even if too big to attach

	ClickURL   string // absolute dashboard link
	LiveEntity string // hass only

	Critical bool // loudest treatment; styling only where there is no DND bypass
	// Update replaces a message already delivered, so it must not alert again.
	// Critical still describes the review, so chat backends keep their styling.
	Update bool
}

// Sender delivers a Message to one Target.
type Sender interface {
	// Send delivers m. prev is the ref of an earlier Send to this target, or
	// "". The returned ref is opaque; backends that replace by tag return "".
	Send(ctx context.Context, t config.Target, m Message, prev string) (ref string, err error)
}

// Renderer shows the payload Send would use, for dry runs and cmd/replay.
type Renderer interface {
	Render(t config.Target, m Message) any
}

// Verifier checks targets at startup.
type Verifier interface {
	Verify(ctx context.Context, targets []config.Target) error
}

// Registry maps a target type to its Sender.
type Registry map[config.TargetType]Sender

// For returns the sender for a target's type.
func (r Registry) For(t config.Target) (Sender, error) {
	s, ok := r[t.Type]
	if !ok {
		return nil, fmt.Errorf("no sender configured for target type %q", t.Type)
	}
	return s, nil
}

// Verify runs each Verifier over its targets. Failures are not fatal: they
// must never block startup.
func (r Registry) Verify(ctx context.Context, recipients map[string]config.Recipient) []error {
	byType := map[config.TargetType][]config.Target{}
	for _, rec := range recipients {
		for _, t := range rec.Targets {
			byType[t.Type] = append(byType[t.Type], t)
		}
	}

	var errs []error
	for typ, targets := range byType {
		v, ok := r[typ].(Verifier)
		if !ok {
			continue
		}
		if err := v.Verify(ctx, targets); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", typ, err))
		}
	}
	return errs
}

// New registers a sender per usable backend. Slack and ntfy need credentials,
// which config validation already requires of any target using them.
func New(cfg *config.Config, hass HassClient) Registry {
	r := Registry{
		config.TargetHass:    NewHass(hass, cfg.DashboardPath),
		config.TargetDiscord: NewDiscord(),
	}
	if cfg.Slack.BotToken != "" {
		r[config.TargetSlack] = NewSlack(cfg.Slack)
	}
	if cfg.Ntfy.URL != "" {
		r[config.TargetNtfy] = NewNtfy(cfg.Ntfy)
	}
	return r
}

// duration is how long the review lasted, or "" if either end is unknown.
func duration(m Message) string {
	if m.Start.IsZero() || m.End.IsZero() {
		return ""
	}
	return formatDuration(m.End.Sub(m.Start))
}

// formatDuration writes "42s", "3m 12s", "1h 5m".
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
