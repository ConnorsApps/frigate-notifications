package sender

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// Discord delivers through a channel webhook. The URL is the credential, so
// this uses a plain HTTP client (OTel records URLs). The ref is the message id;
// a PATCH replaces the embeds wholesale, so an edit sends the whole message.
type Discord struct {
	client *http.Client
}

func NewDiscord() *Discord {
	return &Discord{client: &http.Client{Timeout: httpTimeout}}
}

// Limits and colours.
const (
	// Cut before escaping, which at most doubles: keeps content under 2000 and
	// the embed under its 6000 total, with room for links.
	discordHeadlineMax = 400
	discordDetailMax   = 1500
	discordFooterMax   = 300

	discordColorAlert     = 0x3B82F6
	discordColorDetection = 0x868E96
	discordColorSuspect   = 0xF59F00
	discordColorCritical  = 0xE03131

	// Silent post; can only be set on create.
	discordSuppressNotifications = 1 << 12
)

type discordPayload struct {
	Username string `json:"username"`
	// Content is the push preview; embeds may not be shown there.
	Content         string          `json:"content"`
	Embeds          []discordEmbed  `json:"embeds"`
	AllowedMentions discordMentions `json:"allowed_mentions"`
	Flags           int             `json:"flags,omitempty"`
}

type discordEmbed struct {
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Image       *discordImage  `json:"image,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
	// Shown in the viewer's timezone beside the footer.
	Timestamp string `json:"timestamp,omitempty"`
}

type discordImage struct {
	URL string `json:"url"`
}

type discordFooter struct {
	Text string `json:"text"`
}

// discordMentions: an empty Parse makes every mention inert, so model text
// can't ping.
type discordMentions struct {
	Parse []string `json:"parse"`
}

// Render returns the body Send would use.
func (d *Discord) Render(_ config.Target, m Message) any { return d.render(m) }

func (d *Discord) render(m Message) discordPayload {
	title := m.Title
	if m.Critical {
		title = "🚨 " + title
	}
	digest := m.Stage == StageDigest

	content := "**" + discordEscape(truncate(title, 100)) + "** · " + discordEscape(truncate(m.Headline, discordHeadlineMax))

	var links []string
	if m.ClipURL != "" {
		links = append(links, discordLink(m.ClipURL, "▶ View clip"))
	}
	if m.ClickURL != "" {
		links = append(links, discordLink(m.ClickURL, "Dashboard"))
	}
	desc := discordEscape(truncate(m.Detail, discordDetailMax))
	if len(links) > 0 {
		if desc != "" {
			desc += "\n\n"
		}
		desc += strings.Join(links, " · ")
	}

	embed := discordEmbed{
		Description: desc,
		Color:       discordColor(m),
	}
	if m.Image != "" {
		// Re-sent on every edit.
		embed.Image = &discordImage{URL: m.Image}
	}
	if !digest {
		if footer := discordFooterText(m); footer != "" {
			embed.Footer = &discordFooter{Text: footer}
		}
		if !m.Start.IsZero() {
			embed.Timestamp = m.Start.UTC().Format(time.RFC3339)
		}
	}

	// An empty embed is a 400; [] rather than omitted, so an edit clears one.
	embeds := []discordEmbed{}
	if embed.Description != "" || embed.Image != nil || embed.Footer != nil {
		embeds = append(embeds, embed)
	}

	payload := discordPayload{
		Username:        "Frigate",
		Content:         content,
		Embeds:          embeds,
		AllowedMentions: discordMentions{Parse: []string{}},
	}
	if digest {
		payload.Flags = discordSuppressNotifications
	}
	return payload
}

func discordColor(m Message) int {
	switch {
	case m.Critical || m.Threat >= 2:
		return discordColorCritical
	case m.Threat == 1:
		return discordColorSuspect
	case m.Severity == "detection" || m.Stage == StageDigest:
		return discordColorDetection
	}
	return discordColorAlert
}

// discordFooterText is zones, severity, duration.
func discordFooterText(m Message) string {
	var parts []string
	if len(m.Zones) > 0 {
		parts = append(parts, truncate(strings.Join(m.Zones, ", "), discordFooterMax))
	}
	if m.Severity != "" {
		parts = append(parts, capitalize(m.Severity))
	}
	if d := duration(m); d != "" {
		parts = append(parts, d)
	}
	return discordEscape(strings.Join(parts, " · "))
}

// discordEscape backslash-escapes markdown so model text can't restyle the
// message, link, or start a heading. "<" covers mentions and timestamps.
func discordEscape(s string) string {
	s = strings.NewReplacer(
		`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "|", `\|`,
		"`", "\\`", `[`, `\[`, `]`, `\]`, `>`, `\>`, `<`, `\<`,
	).Replace(s)

	// Headings, lists and subtext count only at line start.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if rest := strings.TrimLeft(line, " "); rest != "" && strings.ContainsRune("#-+", rune(rest[0])) {
			lines[i] = line[:len(line)-len(rest)] + `\` + rest
		}
	}
	return strings.Join(lines, "\n")
}

// discordLink is a markdown link; ")" would end it early.
func discordLink(u, label string) string {
	return "[" + label + "](" + strings.ReplaceAll(u, ")", "%29") + ")"
}

// Send posts, or edits the message prev names.
func (d *Discord) Send(ctx context.Context, t config.Target, m Message, prev string) (string, error) {
	body := d.render(m)

	if prev != "" {
		editURL, err := webhookURL(t.WebhookURL, "/messages/"+url.PathEscape(prev), false)
		if err != nil {
			return "", err
		}
		resp, err := d.do(ctx, http.MethodPatch, editURL, body)
		if err != nil {
			return "", fmt.Errorf("discord edit: %w", err)
		}
		switch {
		case resp.ok():
			return prev, nil
		case resp.status == http.StatusNotFound:
			// Deleted: post fresh.
		default:
			return "", resp.err("discord edit")
		}
	}

	postURL, err := webhookURL(t.WebhookURL, "", true)
	if err != nil {
		return "", err
	}
	resp, err := d.do(ctx, http.MethodPost, postURL, body)
	if err != nil {
		return "", fmt.Errorf("discord post: %w", err)
	}
	if !resp.ok() {
		return "", resp.err("discord post")
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.body, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("discord post: reply had no message id")
	}
	return created.ID, nil
}

// do sends a request, retrying once after a 429 (nothing was posted).
func (d *Discord) do(ctx context.Context, method, target string, body discordPayload) (response, error) {
	resp, err := doJSON(ctx, d.client, method, target, "", body)
	if err != nil {
		return response{}, err
	}
	if resp.status == http.StatusTooManyRequests && waitRetryAfter(ctx, resp.retryAfter) {
		return doJSON(ctx, d.client, method, target, "", body)
	}
	return resp, nil
}

// webhookURL adds a path suffix and, for a create, ?wait=true so the id comes
// back. thread_id is kept: an edit inside a thread needs it.
func webhookURL(raw, suffix string, wait bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("discord: invalid webhook URL")
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + suffix
	if wait {
		q := u.Query()
		q.Set("wait", "true")
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// Verify checks each webhook still exists.
func (d *Discord) Verify(ctx context.Context, targets []config.Target) error {
	var bad []string
	for _, t := range targets {
		resp, err := doJSON(ctx, d.client, http.MethodGet, t.WebhookURL, "", nil)
		if err != nil || !resp.ok() {
			bad = append(bad, t.String())
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("webhooks unreachable or deleted: %s", strings.Join(bad, ", "))
	}
	return nil
}
