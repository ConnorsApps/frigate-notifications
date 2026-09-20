// Command replay evaluates captured Frigate reviews against the configured
// rules and prints the decision and the exact notification payload, without
// touching MQTT, Home Assistant, or Valkey: "why did (or didn't) this fire, and
// what would it have sent?", at any wall clock (--at). Several files play in
// order as one review's life, sharing state:
//
//	replay review-new.json review-end.json review-genai.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/ConnorsApps/frigate-notifications/internal/hasscache"
	homelog "github.com/ConnorsApps/frigate-notifications/internal/lib/log"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/notifier"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
	"github.com/ConnorsApps/frigate-notifications/internal/store"
	"github.com/rs/zerolog/log"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	at := flag.String("at", "", `wall clock to evaluate at: "15:04", "2006-01-02T15:04", or empty for now`)
	phase := flag.String("phase", "", `override the review's lifecycle phase: new, update, end, or genai (one file only)`)
	sun := flag.String("sun", "", `solar times for rules using solar windows, e.g. "dawn=06:12,sunrise=06:42,sunset=19:50,dusk=20:20"`)
	flag.Parse()

	if flag.NArg() < 1 || (*phase != "" && flag.NArg() > 1) {
		fmt.Fprintln(os.Stderr, "usage: replay [--config config.yaml] [--at 03:14] [--phase new] <review.json>...")
		os.Exit(2)
	}

	homelog.Setup(homelog.WithLevelStr("debug"))

	cfg := config.MustRead(*configPath)
	// Never call Home Assistant from the replay tool.
	cfg.DryRun = true

	events := make([]frigate.ReviewEvent, flag.NArg())
	for i, path := range flag.Args() {
		event, err := readReview(path)
		if err != nil {
			log.Fatal().Err(err).Str("file", path).Msg("failed to read review")
		}
		if *phase != "" {
			event.Type = frigate.LifecycleType(*phase)
		}
		events[i] = event
	}

	now, err := parseAt(*at, cfg.Location)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid --at")
	}

	ctx := context.Background()

	signer, err := media.SignerFor(cfg.Media)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid media configuration")
	}

	// An offline state reader: entity conditions resolve through the live
	// service's fail-open path. Without --sun a solar window fails open and
	// always matches, which would quietly make a night rule untestable.
	solar, err := parseSun(*sun, now)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid --sun")
	}
	if solar == nil && usesSolar(cfg) {
		fmt.Fprintln(os.Stderr, "warning: rules use solar windows but --sun was not given; those windows fail open and will match")
	}
	states := hasscache.New(offlineStates{solar: solar}, cfg.Location)

	n := notifier.New(cfg, sender.New(cfg, offlineNotifier{}), states, store.New(ctx, "", nil), signer,
		notifier.WithClock(func() time.Time { return now }))

	fmt.Printf("replaying at %s (%s)\n", now.Format("2006-01-02 15:04:05"), cfg.Timezone)
	for i, event := range events {
		fmt.Printf("\n=== %s (%s) ===\n", flag.Arg(i), event.Type)
		n.HandleReview(ctx, event)
	}

	// Holdoffs are real timers, and which rule matched isn't visible from
	// here, so wait out the longest one any rule could have deferred on.
	time.Sleep(50 * time.Millisecond)
	var longest time.Duration
	for _, r := range cfg.Rules {
		if h := r.EffectiveHoldoff(); h > longest {
			longest = h
		}
	}
	if longest > 0 {
		fmt.Printf("\n(waiting out holdoffs, up to %s)\n", longest)
		time.Sleep(longest + 100*time.Millisecond)
	}
}

func readReview(path string) (frigate.ReviewEvent, error) {
	var event frigate.ReviewEvent
	data, err := os.ReadFile(path)
	if err != nil {
		return event, err
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return event, fmt.Errorf("parse review json: %w", err)
	}
	if event.After.ID == "" {
		return event, errors.New("review json has no after.id; is this a review payload?")
	}
	return event, nil
}

// parseAt interprets --at, defaulting the date to today when only a time of
// day is given.
func parseAt(at string, loc *time.Location) (time.Time, error) {
	now := time.Now().In(loc)
	if at == "" {
		return now, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", at, loc); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("15:04", at, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf(`want "15:04" or "2006-01-02T15:04": %w`, err)
	}
	return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc), nil
}

// usesSolar reports whether any configured window needs sun.sun.
func usesSolar(cfg *config.Config) bool {
	for _, r := range cfg.Rules {
		if r.When.Hours != nil && r.When.Hours.NeedsSolar() {
			return true
		}
		for _, u := range r.Unless {
			if u.Hours != nil && u.Hours.NeedsSolar() {
				return true
			}
		}
	}
	for _, r := range cfg.Recipients {
		if r.ActiveHours != nil && r.ActiveHours.NeedsSolar() {
			return true
		}
	}
	return false
}

// parseSun turns "dawn=06:12,dusk=20:20" into a synthetic sun.sun payload.
func parseSun(spec string, day time.Time) ([]byte, error) {
	if spec == "" {
		return nil, nil
	}

	attrs := map[string]string{}
	fields := map[string]string{
		"dawn": "next_dawn", "dusk": "next_dusk",
		"sunrise": "next_rising", "sunset": "next_setting",
	}
	for _, pair := range strings.Split(spec, ",") {
		name, clock, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return nil, fmt.Errorf("want name=HH:MM, got %q", pair)
		}
		key, known := fields[name]
		if !known {
			return nil, fmt.Errorf("unknown solar event %q (want dawn, dusk, sunrise, or sunset)", name)
		}
		t, err := time.ParseInLocation("15:04", clock, day.Location())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		attrs[key] = time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, day.Location()).
			Format(time.RFC3339)
	}

	// All four must be present or the cache rejects the payload.
	for _, key := range fields {
		if attrs[key] == "" {
			return nil, fmt.Errorf("missing solar events: supply all of dawn, dusk, sunrise, sunset")
		}
	}
	return json.Marshal(map[string]any{"attributes": attrs})
}

type offlineStates struct {
	solar []byte
}

func (o offlineStates) EntityState(_ context.Context, entityID string) ([]byte, error) {
	if entityID == "sun.sun" && o.solar != nil {
		return o.solar, nil
	}
	return nil, errors.New("replay: home assistant not contacted")
}

// offlineNotifier stands in for Home Assistant. Replay forces dryRun, so
// nothing is ever sent through it; it only lets the senders be constructed.
type offlineNotifier struct{}

func (offlineNotifier) Notify(context.Context, string, map[string]any) error { return nil }
