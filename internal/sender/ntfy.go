package sender

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// Ntfy delivers through an ntfy server, as JSON: headers are ASCII-only.
// Publishing again with the same sequence_id replaces a notification (server
// >= 2.16, Android >= 1.22.2; iOS shows a second one).
type Ntfy struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewNtfy(cfg config.NtfyConfig) *Ntfy {
	return &Ntfy{
		baseURL: strings.TrimSuffix(cfg.URL, "/"),
		token:   cfg.Token,
		client:  tracedClient(),
	}
}

// ntfyMessage is ntfy's JSON publish body.
type ntfyMessage struct {
	Topic      string       `json:"topic"`
	Title      string       `json:"title,omitempty"`
	Message    string       `json:"message"`
	Priority   int          `json:"priority,omitempty"`
	Tags       []string     `json:"tags,omitempty"`
	Click      string       `json:"click,omitempty"`
	Attach     string       `json:"attach,omitempty"`
	Actions    []ntfyAction `json:"actions,omitempty"`
	SequenceID string       `json:"sequence_id,omitempty"`
}

type ntfyAction struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
	// Clear dismisses the notification once the action is tapped.
	Clear bool `json:"clear,omitempty"`
}

// Priorities: Android gives each a channel the user configures; on iOS 1-2 are
// passive, 4 time-sensitive, 5 critical only if the user opted in.
const (
	ntfyQuiet   = 2
	ntfyDefault = 3
	ntfyHigh    = 4
	ntfyMax     = 5
)

// ntfyEmoji maps a label to an emoji short code, which ntfy prefixes to the
// title. Other labels get "eyes".
var ntfyEmoji = map[string]string{
	"person":  "walking",
	"dog":     "dog",
	"cat":     "cat",
	"car":     "car",
	"package": "package",
}

// Render returns the body Send would publish.
func (n *Ntfy) Render(t config.Target, m Message) any { return n.render(t, m) }

func (n *Ntfy) render(t config.Target, m Message) ntfyMessage {
	msg := ntfyMessage{
		Topic:   t.Topic,
		Title:   m.Title,
		Message: m.Body,
		Click:   m.ClickURL,
		// Clients preview images, not mp4: attach the still, button the clip.
		Attach:     m.Image,
		SequenceID: ntfySequenceID(m.Tag),
	}

	if m.ClipURL != "" {
		// No dashboard button: tapping the notification opens it.
		msg.Actions = append(msg.Actions, ntfyAction{Action: "view", Label: "View Clip", URL: m.ClipURL, Clear: true})
	}

	switch {
	case m.Stage == StageDigest:
		msg.Priority = ntfyQuiet
		msg.Tags = []string{"bar_chart"}
		return msg
	case m.Update:
		// Silent on Android while the first is showing; low priority otherwise.
		msg.Priority = ntfyQuiet
	case m.Critical:
		msg.Priority = ntfyMax
	case m.Severity == "alert":
		msg.Priority = ntfyHigh
	default:
		msg.Priority = ntfyDefault
	}

	if m.Critical {
		msg.Tags = append(msg.Tags, "rotating_light")
	}
	if m.Threat >= 2 {
		msg.Tags = append(msg.Tags, "warning")
	}
	msg.Tags = append(msg.Tags, ntfyObjectTag(m.Objects))
	return msg
}

func ntfyObjectTag(objects []string) string {
	if len(objects) > 0 {
		if tag, ok := ntfyEmoji[objects[0]]; ok {
			return tag
		}
	}
	return "eyes"
}

// ntfySequenceID makes a tag a valid sequence id (letters, digits, "-", "_",
// at most 64); Frigate review ids contain "." and get a 400 otherwise.
func ntfySequenceID(tag string) string {
	id := []rune(tag)
	for i, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			id[i] = '_'
		}
	}
	if len(id) > 64 {
		id = id[:64]
	}
	return string(id)
}

// Send publishes, retrying transport errors, 5xx and 429: a sequence_id makes
// it idempotent.
func (n *Ntfy) Send(ctx context.Context, t config.Target, m Message, _ string) (string, error) {
	body := n.render(t, m)

	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err := doJSON(ctx, n.client, http.MethodPost, n.baseURL+"/", n.token, body)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("ntfy publish: %w", err)
		case resp.ok():
			return "", nil
		case resp.status >= 500 || resp.status == http.StatusTooManyRequests:
			lastErr = resp.err("ntfy publish")
		default:
			// Other 4xx (bad topic, bad token) won't change.
			return "", resp.err("ntfy publish")
		}

		if attempt < attempts {
			select {
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			case <-ctx.Done():
				return "", lastErr
			}
		}
	}
	return "", lastErr
}

// Verify checks the server is reachable; topics can't be checked.
func (n *Ntfy) Verify(ctx context.Context, _ []config.Target) error {
	resp, err := doJSON(ctx, n.client, http.MethodGet, n.baseURL+"/v1/health", n.token, nil)
	if err != nil {
		return fmt.Errorf("ntfy server unreachable: %w", err)
	}
	if !resp.ok() {
		return resp.err("ntfy server health check")
	}
	return nil
}
