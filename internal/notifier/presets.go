package notifier

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/rules"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
)

// mediaProbeBudget bounds all probing for one notification.
const mediaProbeBudget = 15 * time.Second

// candidate is one piece of media a notification could carry.
type candidate struct {
	kind media.Kind
	id   string
}

// mediaCandidates lists what could be attached, best first. Clips exist only
// after the end.
func mediaCandidates(review frigate.ReviewPayload, phase rules.Phase, eventID string) []candidate {
	var out []candidate
	if phase == rules.PhaseEnd {
		if review.EndTime != nil {
			out = append(out, candidate{media.KindClip, media.ClipID(review.Camera, review.StartTime, *review.EndTime)})
		}
		out = append(out, candidate{media.KindPreview, review.ID})
	}
	if eventID != "" {
		out = append(out, candidate{media.KindSnapshot, eventID})
	}
	return out
}

// check asks Frigate whether a candidate exists and fits; with no prober,
// everything does.
func (n *Notifier) check(ctx context.Context, cand candidate) error {
	if n.prober == nil {
		return nil
	}
	err := n.prober.Check(ctx, cand.kind, cand.id, cand.kind.Limit())
	if err != nil {
		n.logger.Info().Err(err).Str("kind", string(cand.kind)).Str("id", cand.id).
			Msg("media not attachable")
	}
	return err
}

// buildContent assembles a review's notification: the best still that fits and
// the clip, if any. Each backend shows what it can.
func (n *Notifier) buildContent(
	ctx context.Context,
	review frigate.ReviewPayload,
	rule config.Rule,
	phase rules.Phase,
	tag, eventID, description string,
) sender.Message {
	c := compose(n.cfg, review, phase, tag, description)

	if cam, ok := n.cfg.Cameras[review.Camera]; ok && rule.Preset == config.PresetLiveView {
		c.LiveEntity = cam.LiveViewEntity
	}

	// No signer: text only, rather than links that 403.
	if n.signer == nil {
		return c
	}

	sign := func(cand candidate) string {
		url, err := n.signer.URL(cand.kind, cand.id)
		if err != nil {
			n.logger.Warn().Err(err).Str("kind", string(cand.kind)).Str("id", cand.id).Msg("failed to sign media url")
			return ""
		}
		return url
	}

	// Probing delays a security push, so it is budgeted; once spent, the rest
	// fail and the push goes out as text.
	ctx, cancel := context.WithTimeout(ctx, mediaProbeBudget)
	defer cancel()

	// The text preset only needs the clip, for "View Clip".
	attach := rule.Preset != config.PresetText
	selected := "none"
	if !attach {
		selected = "off"
	}
	for _, cand := range mediaCandidates(review, phase, eventID) {
		isClip := cand.kind == media.KindClip
		if !attach && !isClip {
			break
		}
		// Later candidates only back up a missing still.
		if !isClip && c.Image != "" {
			break
		}

		err := n.check(ctx, cand)
		// A clip too big to attach can still be linked.
		if err != nil && !(isClip && errors.Is(err, media.ErrTooLarge)) {
			continue
		}
		url := sign(cand)
		if url == "" {
			continue
		}
		if isClip {
			c.ClipURL = url
		}
		if err != nil {
			continue
		}
		if attach {
			if selected == "none" {
				selected = string(cand.kind)
			}
			if isClip {
				c.Video = url
			} else {
				c.Image = url
			}
		}
		// Keep going for a still: chat backends can't play the clip and Android
		// shows only frames of it.
		if isClip && attach {
			continue
		}
		break
	}

	n.metrics.MediaSelected(string(phase), selected)
	return c
}

// compose builds the parts that need neither media nor recipient.
func compose(cfg *config.Config, review frigate.ReviewPayload, phase rules.Phase, tag, description string) sender.Message {
	meta := review.Data.GenAI()

	headline := buildHeadline(review)
	detail := firstSentence(description, maxDescription)
	threat := 0
	if meta != nil {
		threat = meta.PotentialThreatLevel
		if t := strings.TrimSpace(meta.Title); t != "" {
			headline = truncateWords(t, maxTitle)
		}
		// Fuller than the per-object description, so it wins.
		if s := strings.TrimSpace(meta.ShortSummary); s != "" {
			detail = truncateWords(s, maxSummary)
		}
		headline = threatPrefix(threat) + headline
	}

	msg := sender.Message{
		Camera:   review.Camera,
		Title:    cfg.CameraName(review.Camera),
		Headline: headline,
		Detail:   detail,
		Body:     strings.TrimSpace(headline + "\n" + detail),
		Objects:  normalizeObjects(review.Data.Objects),
		Zones:    humanizeAll(review.Data.Zones),
		Severity: string(review.Severity),
		Threat:   threat,
		Stage:    stageFor(phase),
		Tag:      tag,
		ClickURL: cfg.DashboardURL,
	}
	if review.StartTime > 0 {
		msg.Start = unixTime(review.StartTime)
	}
	if review.EndTime != nil {
		msg.End = unixTime(*review.EndTime)
	}
	return msg
}

func stageFor(phase rules.Phase) sender.Stage {
	switch phase {
	case rules.PhaseEnd:
		return sender.StageEnded
	case rules.PhaseUpdate:
		return sender.StageUpdated
	}
	return sender.StageStarted
}

// unixTime converts Frigate's fractional epoch seconds.
func unixTime(sec float64) time.Time {
	whole := int64(sec)
	return time.Unix(whole, int64((sec-float64(whole))*1e9))
}

// threatPrefix marks a GenAI verdict; the wording is Frigate's own.
func threatPrefix(level int) string {
	switch level {
	case 1:
		return "Needs review: "
	case 2:
		return "Security concern: "
	}
	return ""
}

// buildHeadline says what the review reported.
func buildHeadline(review frigate.ReviewPayload) string {
	data := review.Data
	if len(data.Objects) == 0 && len(data.SubLabels) == 0 && len(data.Audio) > 0 {
		// Sound alone.
		return humanizeList(data.Audio) + " heard"
	}

	msg := buildSubject(data) + " detected"
	if len(data.Zones) > 0 {
		msg += " in " + humanizeList(data.Zones)
	}
	return msg
}

// normalizeObjects drops "-verified" (Frigate's mark on recognized objects)
// and repeats.
func normalizeObjects(objects []string) []string {
	out := make([]string, 0, len(objects))
	for _, o := range objects {
		o = strings.TrimSuffix(o, "-verified")
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}

func humanizeAll(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, humanize(item))
	}
	return out
}

// buildSubject names what a review saw. sub_labels can cover fewer people than
// were detected, so the rest are named too.
func buildSubject(data frigate.ReviewData) string {
	if len(data.SubLabels) == 0 {
		objects := normalizeObjects(data.Objects)
		if len(objects) == 0 {
			return "Activity"
		}
		return humanizeList(objects)
	}

	parts := humanizeAll(data.SubLabels)

	toRecognize := len(data.SubLabels)
	unnamed := 0
	var others []string
	for _, obj := range data.Objects {
		// "person-verified" is still a person.
		obj = strings.TrimSuffix(obj, "-verified")
		if obj != "person" {
			others = append(others, obj)
			continue
		}
		if toRecognize > 0 {
			toRecognize--
			continue
		}
		unnamed++
	}

	switch {
	case unnamed == 1:
		parts = append(parts, "Another Person")
	case unnamed > 1:
		parts = append(parts, fmt.Sprintf("%d More People", unnamed))
	}
	for _, obj := range others {
		parts = append(parts, humanize(obj))
	}

	return joinList(parts)
}

// Length bounds: the object description is one short sentence; the GenAI title
// is asked to be 80 but validated softly; the summary may run two sentences.
const (
	maxDescription = 140
	maxTitle       = 120
	maxSummary     = 200
)

// abbreviations end in a "." that isn't a sentence end.
var abbreviations = map[string]bool{"mr": true, "mrs": true, "ms": true, "dr": true, "st": true, "vs": true, "jr": true, "sr": true}

// firstSentence returns s up to its first ".", "!" or "?" that ends the string
// or precedes whitespace (and isn't an abbreviation), cut to max on a word
// boundary.
func firstSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	for i, r := range s {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		rest := s[i+1:]
		if rest != "" && rest[0] != ' ' && rest[0] != '\n' {
			continue
		}
		if r == '.' && abbreviations[strings.ToLower(lastWord(s[:i]))] {
			continue
		}
		s = s[:i+1]
		break
	}
	return truncateWords(s, max)
}

// lastWord is the run of letters at the end of s.
func lastWord(s string) string {
	return s[len(strings.TrimRightFunc(s, unicode.IsLetter)):]
}

// truncateWords cuts s to max runes at a word boundary, with an ellipsis.
func truncateWords(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	cut := r[:max]
	// Back up to a space, unless that loses over half.
	if i := lastSpace(cut); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(string(cut)) + "…"
}

func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if unicode.IsSpace(r[i]) {
			return i
		}
	}
	return -1
}

// humanizeList turns snake_case ids into prose: ["person","dog"] -> "Person
// and Dog".
func humanizeList(items []string) string { return joinList(humanizeAll(items)) }

// joinList joins phrases: "A", "A and B", "A, B, and C".
func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

func humanize(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' })
	for i, w := range words {
		r := []rune(w)
		words[i] = string(unicode.ToUpper(r[0])) + string(r[1:])
	}
	return strings.Join(words, " ")
}
