package main

import (
	"cmp"
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
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/notifier"
	"github.com/ConnorsApps/frigate-notifications/internal/rules"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
)

// reviewMessages builds the new-phase notification for a real Frigate review
// and its end-of-review update, through the notifier's own media selection,
// so every link is one the proxy will serve.
func reviewMessages(ctx context.Context, cfg *config.Config, id string, preset config.Preset, tag string) (first, last sender.Message, err error) {
	signer, err := media.SignerFor(cfg.Media)
	if signer == nil {
		return first, last, cmp.Or(err, errors.New("--review needs media.frigateURL, media.publicBaseURL and media.signingKey"))
	}
	review, err := fetchReview(ctx, cfg.Media.FrigateURL, id)
	if err != nil {
		return first, last, err
	}
	if review.EndTime == nil {
		fmt.Printf("review %s is still in progress: the update will have no clip yet\n", id)
	}

	n := notifier.New(cfg, nil, nil, nil, signer, notifier.WithMediaProber(media.NewProber(cfg.Media.FrigateURL)))
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
