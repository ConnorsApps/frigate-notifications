package sender

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// HassClient is the part of *hass.Client the sender uses.
type HassClient interface {
	Notify(ctx context.Context, service string, data map[string]any) error
}

// Hass delivers through notify.<service>.
type Hass struct {
	client        HassClient
	dashboardPath string
}

// NewHass returns the sender. dashboardPath is the relative path a tap opens.
func NewHass(client HassClient, dashboardPath string) *Hass {
	return &Hass{client: client, dashboardPath: dashboardPath}
}

// Send calls the notify service. It replaces by tag, so there is no ref.
func (h *Hass) Send(ctx context.Context, t config.Target, m Message, _ string) (string, error) {
	return "", h.client.Notify(ctx, t.Service, h.payload(m))
}

// Render returns the call Send would make.
func (h *Hass) Render(_ config.Target, m Message) any { return h.payload(m) }

// Android channels. Importance is fixed when the app first sees a channel, so
// each behaviour gets its own.
const (
	hassChannel         = "Frigate"
	hassChannelDigest   = "Frigate Digest"
	hassChannelCritical = "alarm_stream" // rings regardless of ringer mode
)

// payload carries iOS and Android keys together; each ignores the other's.
func (h *Hass) payload(m Message) map[string]any {
	data := map[string]any{
		"tag": m.Tag,
		// Android skips sound when replacing a same-tag notification.
		"alert_once": true,
	}

	if m.Camera != "" {
		data["group"] = "frigate-" + m.Camera
	}
	if sub := hassSubtitle(m); sub != "" {
		data["subtitle"] = sub
	}
	if !m.Start.IsZero() {
		data["when"] = m.Start.Unix()
	}
	data["notification_icon"] = "mdi:cctv"

	if h.dashboardPath != "" {
		data["clickAction"] = h.dashboardPath
		data["url"] = h.dashboardPath
	}

	// Android shows a "video" as a few frames and never a picture; iOS plays an
	// attachment, which outranks "image". Send both; without a still, the clip
	// goes alone.
	switch {
	case m.Video != "" && m.Image != "":
		data["image"] = m.Image
		data["attachment"] = map[string]any{"url": m.Video, "content-type": "video/mp4"}
	case m.Video != "":
		data["video"] = m.Video
	case m.Image != "":
		data["image"] = m.Image
	}
	if m.LiveEntity != "" {
		data["entity_id"] = m.LiveEntity
	}

	if m.ClipURL != "" {
		data["actions"] = []map[string]any{{
			"action": "URI",
			"title":  "View Clip",
			"uri":    m.ClipURL,
		}}
	}

	switch {
	case m.Stage == StageDigest:
		data["channel"] = hassChannelDigest
		data["importance"] = "low"
		data["push"] = map[string]any{"interruption-level": "passive"}

	case m.Update:
		// Critical is dropped: iOS can't replace a critical notification, so
		// repeating it would ring twice.
		data["channel"] = hassChannel
		data["push"] = map[string]any{
			"interruption-level": "passive",
			"sound":              "none",
		}

	case m.Critical:
		data["push"] = map[string]any{
			"interruption-level": "critical",
			"sound": map[string]any{
				"name":     "default",
				"critical": 1,
				"volume":   1.0,
			},
		}
		data["channel"] = hassChannelCritical
		data["ttl"] = 0
		data["priority"] = "high"
		data["importance"] = "high"
		data["color"] = "#D32F2F"

	default:
		data["channel"] = hassChannel
		data["importance"] = "high"
		data["priority"] = "high"
		if m.Severity == "alert" {
			// Alerts break through Focus; detections don't.
			data["push"] = map[string]any{"interruption-level": "time-sensitive"}
		}
	}

	return map[string]any{
		"title":   m.Title,
		"message": m.Body,
		"data":    data,
	}
}

// hassSubtitle is the iOS line under the title: severity, zones.
func hassSubtitle(m Message) string {
	var parts []string
	if m.Severity != "" {
		parts = append(parts, capitalize(m.Severity))
	}
	parts = append(parts, m.Zones...)
	return strings.Join(parts, " · ")
}

// serviceLister is implemented by *hass.Client.
type serviceLister interface {
	NotifyServices(ctx context.Context) (map[string]bool, error)
}

// Verify reports notify services Home Assistant doesn't expose.
func (h *Hass) Verify(ctx context.Context, targets []config.Target) error {
	lister, ok := h.client.(serviceLister)
	if !ok {
		return nil
	}
	available, err := lister.NotifyServices(ctx)
	if err != nil {
		return fmt.Errorf("could not verify notify services against home assistant: %w", err)
	}

	var missing []string
	for _, t := range targets {
		if !available[t.Service] {
			missing = append(missing, "notify."+t.Service)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("home assistant does not expose %s; notifications to these targets will fail", strings.Join(missing, ", "))
}
