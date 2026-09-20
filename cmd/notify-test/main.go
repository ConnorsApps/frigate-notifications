// Command notify-test sends one real notification to a recipient's targets,
// bypassing MQTT, Frigate, and every rule in config.yaml, to check by hand that
// it reaches the device. Home Assistant's notify call succeeds once it hands
// the push to APNs/FCM, so a 200 alone doesn't prove the phone rang.
package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/lib/hass"
	homelog "github.com/ConnorsApps/frigate-notifications/internal/lib/log"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
)

func main() {
	configPath := flag.String("config", "config.yaml", "config.yaml to read the recipient's targets and backend credentials from")
	recipient := flag.String("recipient", "alice", "recipient name from config.yaml's recipients map")
	target := flag.String("target", "", "send only to this backend (hass, slack, ntfy, discord); default is every target the recipient has")
	title := flag.String("title", "notify-test", "notification title")
	message := flag.String("message", "manual test notification from frigate-notify's notify-test tool", "notification body")
	critical := flag.Bool("critical", false, "send the critical form of the notification (hass: max volume, bypasses Do Not Disturb / silent mode / most Focus modes)")
	image := flag.String("image", "", "attach this image URL (JPEG/GIF/PNG, ≤10 MB)")
	video := flag.String("video", "", "attach this video URL (MP4, ≤50 MB); backends that can't play video link it instead")
	tag := flag.String("tag", "notify-test", "notification tag/group; sending again with the same tag updates the notification in place where the backend supports it")
	updateAfter := flag.Duration("update-after", 0, "after sending, wait this long and send the end-of-review update (clip ready, GenAI text) to the same message, as a real review does; shows whether each backend edits in place and stays quiet")
	list := flag.Bool("list-services", false, "list notify.* services Home Assistant exposes, then exit")
	flag.Parse()

	homelog.Setup(homelog.WithLevelStr("info"))

	cfg := config.MustRead(*configPath)

	hassClient := hass.New(cfg.Hass.URL, cfg.Hass.Token)
	ctx := context.Background()

	if *list {
		services, err := hassClient.NotifyServices(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "notify-test: %v\n", err)
			os.Exit(1)
		}
		for _, name := range slices.Sorted(maps.Keys(services)) {
			fmt.Println(name)
		}
		return
	}

	r, ok := cfg.Recipients[*recipient]
	if !ok {
		fmt.Fprintf(os.Stderr, "notify-test: unknown recipient %q\n", *recipient)
		os.Exit(1)
	}

	senders := sender.New(cfg, hassClient)

	// A typo'd service name or bad token otherwise fails with an opaque
	// error from the backend; check first and say so.
	for _, err := range senders.Verify(ctx, map[string]config.Recipient{*recipient: r}) {
		fmt.Fprintf(os.Stderr, "notify-test: warning: %v\n", err)
	}

	start := time.Now()
	msg := sender.Message{
		Camera:   "notify_test",
		Title:    *title,
		Headline: *message,
		Body:     *message,
		Objects:  []string{"person"},
		Zones:    []string{"Test Zone"},
		Severity: "alert",
		Stage:    sender.StageStarted,
		Start:    start,
		Tag:      *tag,
		Image:    *image,
		ClickURL: cfg.DashboardURL,
		Critical: *critical,
	}
	if *updateAfter == 0 {
		// One message with everything on it.
		msg.Video, msg.ClipURL = *video, *video
	}

	type delivery struct {
		target config.Target
		s      sender.Sender
		ref    string
	}
	var sent []delivery
	failed := 0
	for _, t := range r.Targets {
		if *target != "" && string(t.Type) != *target {
			continue
		}
		s, err := senders.For(t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "notify-test: %s: %v\n", t, err)
			failed++
			continue
		}
		fmt.Printf("sending to %s (critical=%v)\n", t, *critical)
		ref, err := s.Send(ctx, t, msg, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "notify-test: %s: send failed: %v\n", t, err)
			failed++
			continue
		}
		fmt.Printf("%s accepted the request\n", t.Type)
		sent = append(sent, delivery{t, s, ref})
	}

	if len(sent)+failed == 0 {
		fmt.Fprintf(os.Stderr, "notify-test: recipient %q has no target matching %q\n", *recipient, *target)
		os.Exit(1)
	}

	if *updateAfter > 0 && len(sent) > 0 {
		fmt.Printf("waiting %s, then sending the update\n", *updateAfter)
		time.Sleep(*updateAfter)

		update := msg
		update.Stage = sender.StageEnded
		update.Update = true
		update.End = time.Now()
		update.Detail = "Test update: the review has ended and the clip is ready."
		update.Body = update.Headline + "\n" + update.Detail
		update.Video, update.ClipURL = *video, *video
		for _, d := range sent {
			fmt.Printf("updating %s\n", d.target)
			if _, err := d.s.Send(ctx, d.target, update, d.ref); err != nil {
				fmt.Fprintf(os.Stderr, "notify-test: %s: update failed: %v\n", d.target, err)
				failed++
			}
		}
	}

	if failed > 0 {
		os.Exit(1)
	}

	if *critical {
		fmt.Println("if the device didn't ring: check that Critical Alerts is toggled on for the")
		fmt.Println("Home Assistant app under iOS Settings > Notifications > Home Assistant — HA")
		fmt.Println("accepting the call only proves it reached HA, not that Apple delivered it.")
	}
}
