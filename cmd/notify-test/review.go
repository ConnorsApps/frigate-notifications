package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/ConnorsApps/frigate-notifications/internal/hasscache"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/notifier"
	"github.com/ConnorsApps/frigate-notifications/internal/rules"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
	"github.com/ConnorsApps/frigate-notifications/internal/store"
)

// reviewMessages builds the new-phase notification for a real Frigate review
// and its end-of-review update, through the notifier's own media selection,
// so every link is one the proxy will serve.
func reviewMessages(ctx context.Context, cfg *config.Config, states hasscache.StateReader, id string, preset config.Preset, tag string) (first, update sender.Message, err error) {
	if !cfg.Media.Enabled() {
		return first, update, errors.New("--review needs media.frigateURL, media.publicBaseURL and media.signingKey")
	}
	signer, err := media.SignerFor(cfg.Media)
	if err != nil {
		return first, update, err
	}
	review, err := fetchReview(ctx, cfg.Media.FrigateURL, id)
	if err != nil {
		return first, update, err
	}
	if _, ok := cfg.Cameras[review.Camera]; !ok {
		return first, update, fmt.Errorf("review %s is on camera %q, which config.yaml doesn't list", id, review.Camera)
	}
	if review.EndTime == nil {
		fmt.Printf("review %s is still in progress: the update will have no clip yet\n", id)
	}

	n := notifier.New(cfg, sender.Registry{}, hasscache.New(states, cfg.Location), store.New(ctx, "", nil), signer,
		notifier.WithMediaProber(media.NewProber(cfg.Media.FrigateURL)))
	rule := config.Rule{Name: "notify-test", Preset: preset}
	return n.Content(ctx, review, rules.PhaseNew, rule, tag), n.Content(ctx, review, rules.PhaseEnd, rule, tag), nil
}

// fetchReview reads one review from Frigate's API, which answers in the shape
// of the MQTT payload's "after".
func fetchReview(ctx context.Context, frigateURL, id string) (frigate.ReviewPayload, error) {
	var review frigate.ReviewPayload
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	target := strings.TrimSuffix(frigateURL, "/") + "/api/review/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return review, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return review, fmt.Errorf("fetch review: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return review, fmt.Errorf("fetch review: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return review, fmt.Errorf("fetch review: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &review); err != nil {
		return review, fmt.Errorf("parse review: %w", err)
	}
	if review.ID == "" {
		return review, errors.New("parse review: no id in Frigate's answer")
	}
	return review, nil
}
