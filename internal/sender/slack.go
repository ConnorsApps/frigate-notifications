package sender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// Slack delivers through the Web API with a bot token: only the posting bot
// can chat.update a message. The ref is "<channel>:<ts>", with the channel
// from Slack's reply, since a post to a user id opens a DM and chat.update
// needs its D... id.
type Slack struct {
	token  string
	apiURL string
	client *http.Client
}

const slackAPI = "https://slack.com/api"

func NewSlack(cfg config.SlackConfig) *Slack {
	return &Slack{token: cfg.BotToken, apiURL: slackAPI, client: tracedClient()}
}

// Slack's field limits; over them the call is rejected.
const (
	slackHeaderMax  = 150
	slackSectionMax = 3000
	slackAltTextMax = 2000
	slackTextMax    = 4000
)

// slackBlock is one Block Kit block.
type slackBlock struct {
	Type     string         `json:"type"`
	Text     *slackText     `json:"text,omitempty"`
	ImageURL string         `json:"image_url,omitempty"`
	AltText  string         `json:"alt_text,omitempty"`
	Elements []slackElement `json:"elements,omitempty"`
}

type slackText struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

type slackElement struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// slackMessage is the body of chat.postMessage and chat.update.
type slackMessage struct {
	Channel string `json:"channel"`
	TS      string `json:"ts,omitempty"`
	// Text is the push preview. Always sent with blocks, or an update keeps
	// the old ones.
	Text        string       `json:"text"`
	Blocks      []slackBlock `json:"blocks"`
	UnfurlLinks bool         `json:"unfurl_links"`
	UnfurlMedia bool         `json:"unfurl_media"`
}

// Render returns the postMessage body Send would use.
func (s *Slack) Render(t config.Target, m Message) any { return s.render(t.Channel, "", m, true) }

// render lays out header, text, picture and a context line. Text is mrkdwn
// and may be model-written, so it goes through slackSafe; Slack doesn't
// document whether a mention in plain_text can ping.
func (s *Slack) render(channel, ts string, m Message, withImage bool) slackMessage {
	title := m.Title
	if m.Critical {
		title = "🚨 " + title
	}

	blocks := []slackBlock{
		{Type: "header", Text: &slackText{Type: "plain_text", Text: truncate(title, slackHeaderMax), Emoji: true}},
		{Type: "section", Text: &slackText{Type: "mrkdwn", Text: slackBody(m)}},
	}
	if withImage && m.Image != "" {
		blocks = append(blocks, slackBlock{
			Type:     "image",
			ImageURL: m.Image,
			AltText:  truncate(m.Title+": "+m.Headline, slackAltTextMax),
		})
	}
	if context := slackContext(m); context != "" {
		blocks = append(blocks, slackBlock{
			Type:     "context",
			Elements: []slackElement{{Type: "mrkdwn", Text: context}},
		})
	}

	return slackMessage{
		Channel: channel,
		TS:      ts,
		Text:    slackFit(m.Title+": "+m.Headline, slackTextMax),
		Blocks:  blocks,
	}
}

// slackBody is the bold headline, then the detail.
func slackBody(m Message) string {
	body := "*" + slackFit(m.Headline, 1000) + "*"
	if m.Detail != "" {
		body += "\n" + slackFit(m.Detail, 1900)
	}
	return body
}

// slackFit makes s safe and at most max runes once escaped, cutting between
// entities. Escaping can grow text fivefold, so it can't be cut first.
func slackFit(s string, max int) string {
	if safe := slackSafe(s); utf8.RuneCountInString(safe) <= max {
		return safe
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		piece := slackSafe(string(r))
		cost := utf8.RuneCountInString(piece)
		if n+cost > max-1 {
			break
		}
		b.WriteString(piece)
		n += cost
	}
	return strings.TrimSpace(b.String()) + "…"
}

// slackContext is the line under the picture. Links, not buttons: a url
// button still posts an interaction, and an app with no interactivity URL
// shows a warning triangle.
func slackContext(m Message) string {
	var parts []string
	if !m.Start.IsZero() {
		// Rendered in the viewer's timezone.
		parts = append(parts, fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>",
			m.Start.Unix(), m.Start.UTC().Format("Jan 2 15:04 UTC")))
	}
	if d := duration(m); d != "" {
		parts = append(parts, d)
	}
	if len(m.Zones) > 0 {
		parts = append(parts, slackEscape(strings.Join(m.Zones, ", ")))
	}
	if m.Severity != "" {
		parts = append(parts, capitalize(m.Severity))
	}
	if m.ClipURL != "" {
		parts = append(parts, slackLink(m.ClipURL, "View clip"))
	}
	if m.ClickURL != "" {
		parts = append(parts, slackLink(m.ClickURL, "Dashboard"))
	}
	return strings.Join(parts, "  ·  ")
}

// slackEscape stops "<!channel>" and "<@U123>" from pinging.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slackSafe escapes text and swaps the formatting marks for look-alikes:
// mrkdwn can't escape them.
func slackSafe(s string) string {
	return strings.NewReplacer("*", "∗", "_", "‗", "~", "∼", "`", "ˋ").Replace(slackEscape(s))
}

// slackLink is a mrkdwn link; its "&" must be escaped.
func slackLink(url, label string) string {
	return "<" + slackEscape(url) + "|" + label + ">"
}

// slackReply is what these calls read; most failures are HTTP 200, ok:false.
type slackReply struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// Send posts, or edits the message prev names.
func (s *Slack) Send(ctx context.Context, t config.Target, m Message, prev string) (string, error) {
	if prev != "" {
		channel, ts, ok := strings.Cut(prev, ":")
		if !ok || channel == "" || ts == "" {
			return "", fmt.Errorf("slack: malformed message ref %q", prev)
		}

		ref, err := s.call(ctx, "chat.update", s.render(channel, ts, m, true), m)
		if err == nil {
			return ref, nil
		}
		// Deleted: post fresh rather than lose the update.
		if !isSlackError(err, "message_not_found") {
			return "", err
		}
	}

	return s.call(ctx, "chat.postMessage", s.render(t.Channel, "", m, true), m)
}

// slackError is a failure in Slack's reply.
type slackError struct{ code string }

func (e slackError) Error() string { return "slack: " + e.code }

func isSlackError(err error, code string) bool {
	se, ok := errors.AsType[slackError](err)
	return ok && se.code == code
}

// call invokes a Web API method and returns the message's ref.
//
// Slack fetches image URLs at post time and rejects the whole call with
// invalid_blocks if it can't, so a post retries without the image. An edit
// doesn't: that would strip a picture already showing. A 429 retries once.
func (s *Slack) call(ctx context.Context, method string, body slackMessage, m Message) (string, error) {
	rateRetried, imageDropped := false, false

	for {
		resp, err := doJSON(ctx, s.client, http.MethodPost, s.apiURL+"/"+method, s.token, body)
		if err != nil {
			return "", fmt.Errorf("slack %s: %w", method, err)
		}

		if resp.status == http.StatusTooManyRequests {
			if rateRetried || !waitRetryAfter(ctx, resp.retryAfter) {
				return "", fmt.Errorf("slack %s: rate limited", method)
			}
			rateRetried = true
			continue
		}
		if !resp.ok() {
			return "", resp.err("slack " + method)
		}

		var reply slackReply
		if err := json.Unmarshal(resp.body, &reply); err != nil {
			return "", fmt.Errorf("slack %s: unreadable reply: %w", method, err)
		}
		if reply.OK {
			// chat.update omits these; the held ref is right.
			if reply.Channel == "" {
				reply.Channel = body.Channel
			}
			if reply.TS == "" {
				reply.TS = body.TS
			}
			return reply.Channel + ":" + reply.TS, nil
		}

		if reply.Error == "invalid_blocks" && m.Image != "" && !imageDropped && body.TS == "" {
			imageDropped = true
			body = s.render(body.Channel, body.TS, m, false)
			continue
		}
		return "", fmt.Errorf("slack %s: %w", method, slackError{code: reply.Error})
	}
}

// Verify checks the token; membership shows up as not_in_channel on send.
func (s *Slack) Verify(ctx context.Context, _ []config.Target) error {
	resp, err := doJSON(ctx, s.client, http.MethodPost, s.apiURL+"/auth.test", s.token, nil)
	if err != nil {
		return fmt.Errorf("slack unreachable: %w", err)
	}
	var reply slackReply
	if err := json.Unmarshal(resp.body, &reply); err != nil || !reply.OK {
		code := reply.Error
		if code == "" {
			code = fmt.Sprintf("status %d", resp.status)
		}
		return fmt.Errorf("slack auth.test failed: %s", code)
	}
	return nil
}

// truncate cuts s to max runes with an ellipsis.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}
