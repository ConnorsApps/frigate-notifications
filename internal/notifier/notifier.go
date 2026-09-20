// Package notifier turns Frigate review events into notifications on each
// recipient's backends, driven by the ordered rule list in the config.
package notifier

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/eventstore"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/ConnorsApps/frigate-notifications/internal/hasscache"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/rules"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
	"github.com/ConnorsApps/frigate-notifications/internal/store"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// MediaProber is the subset of *media.Prober the notifier depends on.
type MediaProber interface {
	Check(ctx context.Context, kind media.Kind, id string, limit int64) error
}

// targetTimeout bounds one send; targets run concurrently, so a hung backend
// delays no other.
const targetTimeout = 15 * time.Second

// EventStore is the audit-persistence hook: it records every review event,
// description update, and notification delivery attempt for later review. It
// is never a decision input — nil means persistence is simply skipped, and
// every implementation is expected to be best-effort internally (see
// internal/eventstore's package doc).
type EventStore interface {
	SaveReviewEvent(ctx context.Context, review frigate.ReviewPayload, lifecycle frigate.LifecycleType)
	SaveDescriptionUpdate(ctx context.Context, upd frigate.TrackedObjectUpdate)
	SaveNotification(ctx context.Context, rec eventstore.NotificationRecord)
}

// Metrics is the observability hook.
type Metrics interface {
	ReviewReceived(phase, camera string)
	Sent(rule, recipient, backend, preset, phase string)
	// MediaSelected counts the attachment a notification carried; kind is
	// "none" when every candidate failed and it went out as text, "off" when
	// the rule's preset is text.
	MediaSelected(phase, kind string)
	Suppressed(reason, rule, recipient string)
	Unmatched(camera, phase string)
	NotifyError(recipient, backend string)
}

type nopMetrics struct{}

func (nopMetrics) ReviewReceived(string, string)               {}
func (nopMetrics) Sent(string, string, string, string, string) {}
func (nopMetrics) MediaSelected(string, string)                {}
func (nopMetrics) Suppressed(string, string, string)           {}
func (nopMetrics) Unmatched(string, string)                    {}
func (nopMetrics) NotifyError(string, string)                  {}

type Notifier struct {
	cfg     *config.Config
	senders sender.Registry
	states  *hasscache.Cache
	store   *store.Store
	signer  *media.Signer
	prober  MediaProber
	events  EventStore
	metrics Metrics
	logger  zerolog.Logger

	// pending holds in-flight holdoffs. In-process on purpose: a restart
	// costing one notification isn't worth a durable scheduler.
	pendingMu sync.Mutex
	pending   map[string]*pendingDecision

	// now is overridable so the replay tool can evaluate rules at an
	// arbitrary wall clock.
	now func() time.Time
}

type Option func(*Notifier)

func WithMetrics(m Metrics) Option { return func(n *Notifier) { n.metrics = m } }

// WithEventStore attaches the audit-persistence sink. Omitting this option
// leaves events nil, which every hook below treats as "skip".
func WithEventStore(es EventStore) Option { return func(n *Notifier) { n.events = es } }

// WithMediaProber makes media selection check Frigate before attaching, so a
// missing or oversized clip falls back to a lighter kind. Without it every
// candidate is assumed to exist.
func WithMediaProber(p MediaProber) Option { return func(n *Notifier) { n.prober = p } }

// WithClock overrides the notifier's idea of the current time.
func WithClock(fn func() time.Time) Option { return func(n *Notifier) { n.now = fn } }

func New(
	cfg *config.Config,
	senders sender.Registry,
	states *hasscache.Cache,
	st *store.Store,
	signer *media.Signer,
	opts ...Option,
) *Notifier {
	n := &Notifier{
		cfg:     cfg,
		senders: senders,
		states:  states,
		store:   st,
		signer:  signer,
		metrics: nopMetrics{},
		logger:  log.With().Str("logger", "notifier").Logger(),
		pending: make(map[string]*pendingDecision),
		now:     time.Now,
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// HandleReview processes one Frigate review message.
func (n *Notifier) HandleReview(ctx context.Context, event frigate.ReviewEvent) {
	review := event.After
	phase := rules.Phase(event.Type)

	n.metrics.ReviewReceived(string(phase), review.Camera)

	// MQTT is QoS 1, so the broker may hand us the same payload twice. The
	// guard keys on the payload's content, not just (review, phase): Frigate
	// publishes an "update" on every change to a review, so keying on the
	// phase alone would drop every update after the first — including the
	// detection-to-alert escalation that is the reason updates are handled.
	if !n.store.FirstDelivery(ctx, review.ID, string(phase)+":"+reviewFingerprint(review)) {
		n.logger.Debug().Str("reviewId", review.ID).Str("phase", string(phase)).
			Msg("ignoring redelivered review")
		return
	}

	// Audit trail covers every real event, including unconfigured cameras —
	// only the notification decision below is skipped for those.
	if n.events != nil {
		n.events.SaveReviewEvent(ctx, review, event.Type)
	}

	if _, known := n.cfg.Cameras[review.Camera]; !known {
		n.logger.Debug().Str("camera", review.Camera).Msg("ignoring review for unconfigured camera")
		return
	}

	// Not a rule phase: it only refines a notification already sent.
	if event.Type == frigate.LifecycleGenAI {
		n.handleGenAI(ctx, review)
		return
	}

	n.decide(ctx, review, phase, false)
}

// handleGenAI folds the GenAI summary into the notification already sent, as
// a quiet update. It never creates one: whoever was kept out stays out.
func (n *Notifier) handleGenAI(ctx context.Context, review frigate.ReviewPayload) {
	prior, ok := n.store.LoadReview(ctx, review.ID)
	if !ok {
		n.logger.Debug().Str("reviewId", review.ID).Msg("genai summary for a review that was never notified")
		return
	}
	n.sendUpdate(ctx, review, prior, rules.Phase(frigate.LifecycleGenAI))
}

// HandleTrackedObjectUpdate caches GenAI object descriptions. They arrive on
// their own topic, typically after the review has already been notified, and
// are folded into the end-phase update.
func (n *Notifier) HandleTrackedObjectUpdate(ctx context.Context, upd frigate.TrackedObjectUpdate) {
	if upd.Type != frigate.TrackedObjectUpdateDescription || upd.Description == "" {
		return
	}
	if n.events != nil {
		n.events.SaveDescriptionUpdate(ctx, upd)
	}
	n.store.SetDescription(ctx, upd.ID, upd.Description)
	n.logger.Debug().Str("eventId", upd.ID).Msg("cached genai description")
}

// pendingDecision is a decision waiting out its holdoff. It holds the newest
// payload: what the wait is for (a face) arrives as a later update.
type pendingDecision struct {
	timer  *time.Timer
	review frigate.ReviewPayload
	phase  rules.Phase
}

// deferDecision waits out the matched rule's holdoff, so a face lands before
// an irrevocable push. Only that rule waits, not unrelated ones.
func (n *Notifier) deferDecision(ctx context.Context, review frigate.ReviewPayload, phase rules.Phase, delay time.Duration) {
	n.pendingMu.Lock()
	defer n.pendingMu.Unlock()
	if p, exists := n.pending[review.ID]; exists {
		p.review, p.phase = review, phase
		return
	}

	n.logger.Debug().Str("reviewId", review.ID).Str("holdoff", delay.String()).Msg("holding off decision")
	p := &pendingDecision{review: review, phase: phase}
	p.timer = time.AfterFunc(delay, func() {
		n.pendingMu.Lock()
		// An end already decided this review.
		if n.pending[review.ID] != p {
			n.pendingMu.Unlock()
			return
		}
		delete(n.pending, review.ID)
		latest, latestPhase := p.review, p.phase
		n.pendingMu.Unlock()

		if ctx.Err() != nil {
			return
		}
		n.decide(ctx, latest, latestPhase, true)
	})
	n.pending[review.ID] = p
}

// noteLatest records review for a pending holdoff and reports whether one is
// pending. An end stops the wait: its payload is the newest there will be.
func (n *Notifier) noteLatest(review frigate.ReviewPayload, phase rules.Phase) bool {
	n.pendingMu.Lock()
	defer n.pendingMu.Unlock()
	p, ok := n.pending[review.ID]
	if !ok {
		return false
	}
	if phase == rules.PhaseEnd {
		p.timer.Stop()
		delete(n.pending, review.ID)
		return false
	}
	p.review, p.phase = review, phase
	return true
}

// decide matches the review and acts on the result. deferred marks a call
// that has already waited out a holdoff, so it doesn't wait a second time.
func (n *Notifier) decide(ctx context.Context, review frigate.ReviewPayload, phase rules.Phase, deferred bool) {
	// Before matching, so a payload that matches nothing (an excluded person)
	// still replaces the pending one.
	holding := !deferred && n.noteLatest(review, phase)

	mc := n.matchContext(ctx, review, phase)
	idx, trace := rules.Match(n.cfg.Rules, mc)

	logEvent := n.logger.Debug()
	if idx < 0 {
		logEvent = n.logger.Info()
	}
	traceStrings := make([]string, 0, len(trace))
	for _, d := range trace {
		traceStrings = append(traceStrings, d.String())
	}
	logEvent.
		Str("reviewId", review.ID).
		Str("camera", review.Camera).
		Str("phase", string(phase)).
		Strs("trace", traceStrings).
		Msg("rule evaluation")

	if idx < 0 {
		n.metrics.Unmatched(review.Camera, string(phase))
		return
	}
	rule := n.cfg.Rules[idx]

	// Hold off only when the rule that actually matched asks for it. The
	// re-run after the wait may well select a different rule — that is the
	// point, since the signal it waits for is what changes the answer.
	if !deferred && (phase == rules.PhaseNew || holding) {
		if delay := rule.EffectiveHoldoff(); delay > 0 {
			n.deferDecision(ctx, review, phase, delay)
			return
		}
	}

	prior, hasPrior := n.store.LoadReview(ctx, review.ID)
	switch {
	case !hasPrior:
		n.sendNew(ctx, review, phase, idx, rule, false)

	case idx < prior.RuleIndex && rule.Critical && !prior.Critical:
		// A genuine escalation: a higher-priority rule matches now and it
		// raises the notification to critical — most often a review Frigate
		// moved from detection to alert. Re-push rather than edit in place,
		// because the point is to wake someone the quiet version didn't. A
		// lower-priority later match can never reach this branch, so it can
		// never demote a critical alert.
		n.logger.Info().
			Str("reviewId", review.ID).
			Str("from", prior.RuleName).
			Str("to", rule.Name).
			Msg("escalating review to a higher-priority critical rule")
		n.sendNew(ctx, review, phase, idx, rule, true)

	case idx < prior.RuleIndex:
		// A higher-priority rule matches, but it isn't a criticality
		// escalation — typically a rule keyed on a zone or sub-label that
		// only populated after the first push. Refresh the notification
		// already on the phone under the better rule instead of buzzing a
		// second time.
		n.logger.Info().
			Str("reviewId", review.ID).
			Str("from", prior.RuleName).
			Str("to", rule.Name).
			Msg("refreshing review under a higher-priority rule")
		n.refresh(ctx, review, phase, phase, prior, idx, rule)

	case phase == rules.PhaseEnd:
		// Same rule (or a lower-priority one): refresh the notification we
		// already sent rather than sending a second one.
		n.sendUpdate(ctx, review, prior, rules.PhaseEnd)

	default:
		n.logger.Debug().Str("reviewId", review.ID).Msg("already notified; nothing to do")
	}
}

func (n *Notifier) matchContext(ctx context.Context, review frigate.ReviewPayload, phase rules.Phase) rules.MatchContext {
	now := n.now().In(n.cfg.Location)

	var dwell time.Duration
	if review.StartTime > 0 {
		start := time.Unix(int64(review.StartTime), 0)
		if end := review.EndTime; end != nil {
			dwell = time.Unix(int64(*end), 0).Sub(start)
		} else {
			dwell = now.Sub(start)
		}
	}

	return rules.MatchContext{
		Camera:    review.Camera,
		Labels:    review.Data.Objects,
		SubLabels: review.Data.SubLabels,
		Zones:     review.Data.Zones,
		Severity:  string(review.Severity),
		Phase:     phase,
		Now:       now,
		Dwell:     dwell,
		EntityState: func(entityID string) (string, error) {
			return n.states.State(ctx, entityID)
		},
		Solar: func() (config.SolarTimes, error) {
			return n.states.Solar(ctx)
		},
	}
}

// cachedDescription returns the GenAI description cached for eventID, or ""
// if none has arrived yet (including eventID == "").
func (n *Notifier) cachedDescription(ctx context.Context, eventID string) string {
	if eventID == "" {
		return ""
	}
	return n.store.Description(ctx, eventID)
}

// sendNew delivers a first or escalated notification. An escalation gets a new
// tag: Android alert_once and ntfy's replace are silent, and it must be heard.
// The quieter one stays.
func (n *Notifier) sendNew(ctx context.Context, review frigate.ReviewPayload, phase rules.Phase, idx int, rule config.Rule, escalation bool) {
	scopeKey := cooldownKey(rule, idx, review.Camera)
	if !n.store.AcquireCooldown(ctx, scopeKey, time.Duration(rule.Cooldown)) {
		n.logger.Debug().Str("reviewId", review.ID).Str("rule", rule.Name).
			Str("scope", scopeKey).Msg("suppressed by cooldown")
		n.metrics.Suppressed("cooldown", rule.Name, "")
		return
	}

	tag := "fn-" + review.ID
	if escalation {
		tag += "-esc"
	}
	eventID := review.PrimaryEventID()
	content := n.buildContent(ctx, review, rule, phase, tag, eventID, n.cachedDescription(ctx, eventID))

	delivered := n.fanOut(ctx, review, rule, content, phase, eventID)
	if len(delivered) == 0 {
		return
	}

	// An end arriving before this write finds no state and sends a duplicate;
	// the per-target timeout keeps that window short.
	n.store.SaveReview(ctx, review.ID, store.ReviewState{
		RuleIndex:  idx,
		RuleName:   rule.Name,
		Tag:        tag,
		Deliveries: delivered,
		Critical:   rule.Critical,
		EventID:    eventID,
	})
}

// sendUpdate refreshes an already-delivered notification in place under the
// rule it was sent with, adding the clip and any GenAI description that only
// exist once the review has ended.
func (n *Notifier) sendUpdate(ctx context.Context, review frigate.ReviewPayload, prior store.ReviewState, label rules.Phase) {
	idx := prior.RuleIndex
	if idx < 0 || idx >= len(n.cfg.Rules) {
		// The rule list changed under a live review.
		return
	}
	n.refresh(ctx, review, rules.PhaseEnd, label, prior, idx, n.cfg.Rules[idx])
}

// refresh re-renders an already-delivered notification under rule, editing it
// in place (same tag) rather than sending a second push. Only the recipients
// who received the original are touched. When rule is a newly matched
// higher-priority one (idx differs from prior's), it is remembered so a later,
// even-higher match still compares against it.
func (n *Notifier) refresh(ctx context.Context, review frigate.ReviewPayload, contentPhase, label rules.Phase, prior store.ReviewState, idx int, rule config.Rule) {
	eventID := cmp.Or(prior.EventID, review.PrimaryEventID())
	content := n.buildContent(ctx, review, rule, contentPhase, prior.Tag, eventID, n.cachedDescription(ctx, eventID))

	next, changed := n.redeliver(ctx, review.ID, eventID, prior, content, rule, label)
	if idx != prior.RuleIndex {
		next.RuleIndex, next.RuleName, changed = idx, rule.Name, true
	}
	if changed {
		n.store.SaveReview(ctx, review.ID, next)
	}
}

// redeliver resends c to the targets that got the original, with their refs so
// it edits. An update can't create a notification for someone policy kept out.
// It returns the state with any changed refs (an edit that re-posted) and
// whether any changed.
func (n *Notifier) redeliver(
	ctx context.Context,
	reviewID, eventID string,
	prior store.ReviewState,
	c sender.Message,
	rule config.Rule,
	phase rules.Phase,
) (store.ReviewState, bool) {
	next := prior
	next.Deliveries = append([]store.Delivery(nil), prior.Deliveries...)

	names := prior.Recipients()
	changed := make([]bool, len(names))
	var wg sync.WaitGroup
	for k, name := range names {
		recipient, ok := n.cfg.Recipients[name]
		if !ok {
			continue
		}
		wg.Go(func() {
			// A refresh under a critical rule keeps critical styling.
			critical := recipient.AllowCritical && rule.Critical

			var jobs []job
			var at []int // index into next.Deliveries for each job
			for i, d := range prior.Deliveries {
				// The target list can shrink mid-review.
				if d.Recipient == name && d.Target < len(recipient.Targets) {
					jobs = append(jobs, job{target: d.Target, prev: d.Ref})
					at = append(at, i)
				}
			}
			for i, r := range n.deliverTo(ctx, reviewID, eventID, name, recipient, jobs, c, critical, true, rule, phase) {
				// Disjoint entries per recipient: no lock.
				if r.ok && r.ref != jobs[i].prev {
					next.Deliveries[at[i]].Ref = r.ref
					changed[k] = true
				}
			}
		})
	}
	wg.Wait()

	for _, c := range changed {
		if c {
			return next, true
		}
	}
	return next, false
}

// fanOut applies policy and delivers, concurrently per recipient, returning
// what was sent.
func (n *Notifier) fanOut(ctx context.Context, review frigate.ReviewPayload, rule config.Rule, c sender.Message, phase rules.Phase, eventID string) []store.Delivery {
	perRecipient := make([][]store.Delivery, len(rule.To))

	var wg sync.WaitGroup
	for i, name := range rule.To {
		recipient, ok := n.cfg.Recipients[name]
		if !ok {
			continue
		}
		wg.Go(func() {
			verdict := n.applyPolicy(ctx, name, recipient, rule, review.Camera)
			if !verdict.send {
				n.logger.Debug().
					Str("reviewId", review.ID).Str("recipient", name).Str("reason", verdict.reason).
					Msg("suppressed by recipient policy")
				n.metrics.Suppressed(verdict.reason, rule.Name, name)
				return
			}

			jobs := make([]job, len(recipient.Targets))
			for t := range jobs {
				jobs[t] = job{target: t}
			}
			for t, r := range n.deliverTo(ctx, review.ID, eventID, name, recipient, jobs, c, verdict.critical, false, rule, phase) {
				if r.ok {
					perRecipient[i] = append(perRecipient[i], store.Delivery{Recipient: name, Target: t, Ref: r.ref})
				}
			}
		})
	}
	wg.Wait()

	// In rule order, so state doesn't depend on goroutine timing.
	var delivered []store.Delivery
	for _, d := range perRecipient {
		delivered = append(delivered, d...)
	}
	return delivered
}

// job is one send to a target; prev is the ref of the message there, if any.
type job struct {
	target int
	prev   string
}

// sendResult is a job's outcome.
type sendResult struct {
	ok  bool
	ref string
}

// deliverTo sends c to a recipient's targets concurrently, returning outcomes
// in job order. Every attempt is audited, failures and dry runs included.
func (n *Notifier) deliverTo(
	ctx context.Context,
	reviewID, eventID, name string,
	recipient config.Recipient,
	jobs []job,
	c sender.Message,
	critical, update bool,
	rule config.Rule,
	phase rules.Phase,
) []sendResult {
	// critical is per recipient: policy can downgrade it.
	msg := c
	msg.Critical = critical
	msg.Update = update
	out := make([]sendResult, len(jobs))

	var wg sync.WaitGroup
	for i, j := range jobs {
		t := recipient.Targets[j.target]
		wg.Go(func() {
			rec := eventstore.NotificationRecord{
				SentAt:     n.now(),
				ReviewID:   reviewID,
				EventID:    eventID,
				Rule:       rule.Name,
				Recipient:  name,
				Backend:    string(t.Type),
				Target:     t.String(),
				Phase:      string(phase),
				Preset:     string(rule.Preset),
				Critical:   critical,
				DryRun:     n.cfg.DryRun,
				Title:      c.Title,
				Message:    c.Body,
				Tag:        c.Tag,
				Image:      c.Image,
				Video:      c.Video,
				LiveEntity: c.LiveEntity,
				ClipURL:    c.ClipURL,
			}

			ref, err := n.sendOne(ctx, name, t, msg, j.prev)
			switch {
			case err != nil:
				n.logger.Error().Err(err).
					Str("recipient", name).Str("target", t.String()).
					Msg("failed to send notification")
				n.metrics.NotifyError(name, string(t.Type))
				rec.Error = err.Error()
			default:
				out[i] = sendResult{ok: true, ref: ref}
				if !n.cfg.DryRun {
					n.metrics.Sent(rule.Name, name, string(t.Type), string(rule.Preset), string(phase))
				}
			}

			rec.Success = err == nil
			if n.events != nil {
				n.events.SaveNotification(ctx, rec)
			}
		})
	}
	wg.Wait()
	return out
}

// sendOne delivers m, or under dryRun only logs it. The returned ref replaces
// prev.
func (n *Notifier) sendOne(ctx context.Context, recipient string, t config.Target, m sender.Message, prev string) (string, error) {
	s, err := n.senders.For(t)
	if err != nil {
		return "", err
	}

	if n.cfg.DryRun {
		// The backend's real payload, not the neutral message.
		var payload any = m
		if r, ok := s.(sender.Renderer); ok {
			payload = r.Render(t, m)
		}
		n.logger.Info().
			Str("recipient", recipient).
			Str("target", t.String()).
			Interface("payload", payload).
			Msg("dry run: notification not sent")
		return prev, nil
	}

	ctx, cancel := context.WithTimeout(ctx, targetTimeout)
	defer cancel()
	return s.Send(ctx, t, m, prev)
}

// cooldownKey builds the key a rule's cooldown is counted against.
func cooldownKey(rule config.Rule, idx int, camera string) string {
	switch rule.CooldownScope {
	case config.ScopeGlobal:
		return "global"
	case config.ScopeRule:
		return rule.Name
	default:
		return rule.Name + ":" + camera
	}
}

// reviewFingerprint identifies a review's *content*, so a genuinely changed
// republish is processed while a broker redelivery of the same bytes is not.
// It covers exactly the fields any rule can match on; anything else changing
// cannot alter a decision, and would only cost a duplicate push.
func reviewFingerprint(review frigate.ReviewPayload) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|", review.Camera, review.Severity)
	for _, list := range [][]string{
		review.Data.Objects,
		review.Data.SubLabels,
		review.Data.Zones,
		review.Data.Detections,
	} {
		sorted := append([]string(nil), list...)
		sort.Strings(sorted)
		fmt.Fprintf(h, "%s;", strings.Join(sorted, ","))
	}
	// A "genai" message differs from the "end" before it only here.
	if m := review.Data.GenAI(); m != nil {
		fmt.Fprintf(h, "%s|%s|%d", m.Title, m.ShortSummary, m.PotentialThreatLevel)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
