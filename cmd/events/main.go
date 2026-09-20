// Command events summarises what frigate-notify actually did over a recent
// window, read-only from the audit store (MongoDB or PostgreSQL): which rules
// fired, to whom, how they were delivered, how long after the review, and what
// the messages looked like. The counterpart to cmd/replay, which answers "what
// would this review do?".
package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/db"
	"github.com/ConnorsApps/frigate-notifications/internal/eventstore"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "config.yaml to read db.url from (fallback only)")
	dbURL := flag.String("db", os.Getenv("DB_URL"), "MongoDB or PostgreSQL URL (default $DB_URL, then --config)")
	since := flag.Duration("since", 24*time.Hour, "how far back to look")
	ruleFilter := flag.String("rule", "", "only notifications from this rule")
	recipientFilter := flag.String("recipient", "", "only notifications to this recipient")
	cameraFilter := flag.String("camera", "", "only this camera")
	limit := flag.Int("limit", 40, "how many recent notifications to list")
	flag.Parse()

	uri := *dbURL
	if uri == "" {
		if cfg, err := config.Read(*cfgPath); err == nil {
			uri = cfg.DB.URL
		}
	}
	if uri == "" {
		fmt.Fprintln(os.Stderr, "no db URL: pass --db, set $DB_URL, or point --config at a config.yaml with db.url")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := db.Open(ctx, uri, nil, db.ReadOnly())
	if err != nil {
		fatal("connect", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	cutoff := time.Now().Add(-*since)

	reviews, err := store.Reviews(ctx, cutoff, *cameraFilter)
	if err != nil {
		fatal("query reviews", err)
	}
	descriptions, err := store.CountDescriptions(ctx, cutoff)
	if err != nil {
		fatal("count descriptions", err)
	}
	notes, err := store.Notifications(ctx, cutoff, *ruleFilter, *recipientFilter)
	if err != nil {
		fatal("query notifications", err)
	}

	if *cameraFilter != "" {
		cam := map[string]string{}
		for _, r := range reviews {
			cam[r.ReviewID] = r.Camera
		}
		kept := notes[:0]
		for _, n := range notes {
			if cam[n.ReviewID] == *cameraFilter {
				kept = append(kept, n)
			}
		}
		notes = kept
	}

	fmt.Printf("frigate-notify events — last %s (since %s)\n\n", since.String(), cutoff.Format(time.RFC3339))
	printReviews(reviews, descriptions, *since)
	printNotifications(notes)
	printLatency(reviews, notes)
	printRecent(notes, *limit)
	fmt.Println("suppressed (cooldown / active-hours / rate-cap) counts are not persisted here —")
	fmt.Println("see frigate_notify_suppressed_total in Grafana.")
}

func printReviews(reviews []eventstore.ReviewEventRecord, descriptions int64, since time.Duration) {
	fmt.Printf("REVIEWS  %d over %s (%.1f/h), %d GenAI descriptions\n",
		len(reviews), since.String(), float64(len(reviews))/since.Hours(), descriptions)
	if len(reviews) == 0 {
		fmt.Println()
		return
	}
	type key struct{ camera, lifecycle, severity string }
	counts := map[key]int{}
	for _, r := range reviews {
		counts[key{r.Camera, r.LifecycleType, r.Severity}]++
	}
	rows := make([]string, 0, len(counts))
	for k, n := range counts {
		rows = append(rows, fmt.Sprintf("  %5d  %-14s %-8s %s", n, k.camera, k.lifecycle, k.severity))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(rows)))
	for _, r := range rows {
		fmt.Println(r)
	}
	fmt.Println()
}

func printNotifications(notes []eventstore.NotificationRecord) {
	var ok, fail, dry int
	perRecipient := map[string][2]int{} // [sent, ok]
	type key struct {
		rule, recipient, phase, preset string
		critical, success              bool
	}
	counts := map[key]int{}
	for _, n := range notes {
		switch {
		case n.DryRun:
			dry++
		case n.Success:
			ok++
		default:
			fail++
		}
		pr := perRecipient[n.Recipient]
		pr[0]++
		if n.Success {
			pr[1]++
		}
		perRecipient[n.Recipient] = pr
		counts[key{n.Rule, n.Recipient, n.Phase, n.Preset, n.Critical, n.Success}]++
	}

	fmt.Printf("NOTIFICATIONS  %d total — %d ok, %d failed, %d dry-run\n", len(notes), ok, fail, dry)
	if len(notes) == 0 {
		fmt.Println()
		return
	}

	for _, r := range slices.Sorted(maps.Keys(perRecipient)) {
		pr := perRecipient[r]
		rate := 100.0
		if pr[0] > 0 {
			rate = 100 * float64(pr[1]) / float64(pr[0])
		}
		fmt.Printf("  %-14s %3d sent  %3d ok  %5.1f%%\n", r, pr[0], pr[1], rate)
	}

	fmt.Println("  ---")
	rows := make([]string, 0, len(counts))
	for k, n := range counts {
		crit := "-"
		if k.critical {
			crit = "C"
		}
		res := "ok"
		if !k.success {
			res = "ERR"
		}
		rows = append(rows, fmt.Sprintf("  %5d  %-14s %-14s %-6s %-8s %s %s", n, k.rule, k.recipient, k.phase, k.preset, crit, res))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(rows)))
	for _, r := range rows {
		fmt.Println(r)
	}

	if fail > 0 {
		fmt.Println("  --- failures ---")
		for _, n := range notes {
			if n.Success || n.DryRun {
				continue
			}
			fmt.Printf("  %s  %-14s %-14s %s\n", n.SentAt.Format("01-02 15:04:05"), n.Recipient, n.Rule, oneLine(n.Error, 100))
		}
	}
	fmt.Println()
}

func printLatency(reviews []eventstore.ReviewEventRecord, notes []eventstore.NotificationRecord) {
	firstReview := map[string]time.Time{}
	for _, r := range reviews {
		if t, ok := firstReview[r.ReviewID]; !ok || r.ReceivedAt.Before(t) {
			firstReview[r.ReviewID] = r.ReceivedAt
		}
	}
	firstNote := map[string]time.Time{}
	for _, n := range notes {
		if n.DryRun {
			continue
		}
		if t, ok := firstNote[n.ReviewID]; !ok || n.SentAt.Before(t) {
			firstNote[n.ReviewID] = n.SentAt
		}
	}

	var deltas []time.Duration
	for id, nt := range firstNote {
		if rt, ok := firstReview[id]; ok && !nt.Before(rt) {
			deltas = append(deltas, nt.Sub(rt))
		}
	}
	if len(deltas) == 0 {
		return
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	fmt.Printf("LATENCY  review → first push, n=%d — p50 %s  p90 %s  max %s\n\n",
		len(deltas), pctile(deltas, 50).Round(time.Millisecond), pctile(deltas, 90).Round(time.Millisecond), deltas[len(deltas)-1].Round(time.Millisecond))
}

func printRecent(notes []eventstore.NotificationRecord, limit int) {
	fmt.Printf("RECENT  last %d notifications\n", limit)
	for _, n := range notes[max(0, len(notes)-limit):] {
		crit := "-"
		if n.Critical {
			crit = "C"
		}
		res := "ok "
		switch {
		case n.DryRun:
			res = "dry"
		case !n.Success:
			res = "ERR"
		}
		fmt.Printf("  %s  %-13s %-13s %-6s %s %s  %s — %s\n",
			n.SentAt.Format("01-02 15:04:05"), n.Rule, n.Recipient, n.Phase, crit, res,
			oneLine(n.Title, 20), oneLine(n.Message, 90))
	}
}

// pctile returns the p-th percentile of a non-empty sorted slice.
func pctile(sorted []time.Duration, p int) time.Duration { return sorted[p*len(sorted)/100] }

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "events: %s: %v\n", what, err)
	os.Exit(1)
}
