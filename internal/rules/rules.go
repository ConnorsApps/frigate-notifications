// Package rules decides which rule, if any, a Frigate review matches.
//
// It is deliberately free of I/O: Home Assistant lookups and solar times
// arrive as injected functions, so both the matching logic and its fail-open
// paths are testable without a network.
package rules

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// Phase is the lifecycle phase of the review being matched.
type Phase string

const (
	PhaseNew    Phase = "new"
	PhaseUpdate Phase = "update"
	PhaseEnd    Phase = "end"
)

// MatchContext is everything a rule can be evaluated against.
type MatchContext struct {
	Camera    string
	Labels    []string
	SubLabels []string
	Zones     []string
	Severity  string
	Phase     Phase
	// Now is the instant being evaluated. Window checks localize it to each
	// rule's own resolved Location (falling back to the config-wide one),
	// so it need not already be in any particular zone.
	Now   time.Time
	Dwell time.Duration

	// EntityState returns a Home Assistant entity's state string; Solar,
	// today's solar times.
	EntityState func(entityID string) (string, error)
	Solar       func() (config.SolarTimes, error)
}

// Decision records why one rule did or did not match. Without this, "why
// didn't I get a notification?" is unanswerable after the fact.
type Decision struct {
	Index    int
	Name     string
	Matched  bool
	Reason   string
	Suppress string // the unless clause that suppressed it, if any
}

func (d Decision) String() string {
	switch {
	case d.Matched:
		return fmt.Sprintf("rules[%d] %s: MATCH", d.Index, d.Name)
	case d.Suppress != "":
		return fmt.Sprintf("rules[%d] %s: suppressed by unless (%s)", d.Index, d.Name, d.Suppress)
	default:
		return fmt.Sprintf("rules[%d] %s: %s", d.Index, d.Name, d.Reason)
	}
}

// Match returns the index of the first matching rule, or -1, plus the full
// decision trace. Rule order is priority.
func Match(rules []config.Rule, mc MatchContext) (int, []Decision) {
	trace := make([]Decision, 0, len(rules))
	for i, rule := range rules {
		d := matchOne(i, rule, mc)
		trace = append(trace, d)
		if d.Matched {
			return i, trace
		}
	}
	return -1, trace
}

func matchOne(i int, rule config.Rule, mc MatchContext) Decision {
	d := Decision{Index: i, Name: rule.Name}

	// A dwell rule can't be judged at "new": the object hasn't dwelt yet,
	// and first-match-wins would let it shadow a rule that should fire now.
	if mc.Phase == PhaseNew && rule.When.MinDwell > 0 {
		d.Reason = "deferred: minDwell not yet evaluable"
		return d
	}
	if ok, reason := evaluate(rule.When, mc, whenOnError, rule.Location); !ok {
		d.Reason = reason
		return d
	}
	if clause, suppressed := suppressedBy(rule.Unless, mc, rule.Location); suppressed {
		d.Suppress = clause
		return d
	}
	d.Matched = true
	return d
}

// suppressedBy reports whether any unless entry matches. Entries are ORed.
func suppressedBy(unless []config.RuleConditions, mc MatchContext, loc *time.Location) (string, bool) {
	for i, u := range unless {
		if ok, _ := evaluate(u, mc, unlessOnError, loc); ok {
			return fmt.Sprintf("unless[%d]", i), true
		}
	}
	return "", false
}

// onError is the value a condition takes when it can't be resolved (Home
// Assistant unreachable). An infrastructure failure must never cost a
// notification: in `when` it counts as satisfied so the rule can still fire, in
// `unless` as unsatisfied so nothing is suppressed.
const (
	whenOnError   = true
	unlessOnError = false
)

// evaluate ANDs every set condition. The returned reason names the first
// condition that failed.
func evaluate(c config.RuleConditions, mc MatchContext, onError bool, loc *time.Location) (bool, string) {
	if len(c.Cameras) > 0 && !contains(c.Cameras, mc.Camera) {
		return false, "camera " + mc.Camera + " not in cameras"
	}
	if len(c.Labels) > 0 && !anyIn(c.Labels, mc.Labels) {
		return false, "no label in " + strings.Join(c.Labels, ",")
	}
	if len(c.Zones) > 0 && !anyIn(c.Zones, mc.Zones) {
		return false, "no zone in " + strings.Join(c.Zones, ",")
	}
	if len(c.SubLabels) > 0 && !anyIn(c.SubLabels, mc.SubLabels) {
		return false, "no sub-label in " + strings.Join(c.SubLabels, ",")
	}
	if len(c.ExcludeSubLabels) > 0 && excludesEveryone(c.ExcludeSubLabels, mc) {
		return false, "every detected person recognized and excluded"
	}
	if len(c.Severity) > 0 && !contains(c.Severity, mc.Severity) {
		return false, "severity " + mc.Severity + " not in " + strings.Join(c.Severity, ",")
	}
	if c.MinDwell > 0 && mc.Dwell < time.Duration(c.MinDwell) {
		return false, fmt.Sprintf("dwell %s < minDwell %s", mc.Dwell, time.Duration(c.MinDwell))
	}
	if c.Hours != nil {
		if ok, reason := inWindow(*c.Hours, mc, onError, loc); !ok {
			return false, reason
		}
	}
	for entityID, want := range c.EntityState {
		if ok, reason := entityMatches(mc, entityID, want, onError); !ok {
			return false, reason
		}
	}
	return true, ""
}

// inWindow evaluates an hours window, resolving solar endpoints if needed.
// now is localized to loc (the rule's resolved timezone, or the config-wide
// one) before comparing against the window.
//
// If the solar times can't be fetched the answer is onError, so an
// unreachable Home Assistant never silences a night rule.
func inWindow(w config.Window, mc MatchContext, onError bool, loc *time.Location) (bool, string) {
	var st config.SolarTimes
	if w.NeedsSolar() {
		if mc.Solar == nil {
			return onError, "solar times unavailable"
		}
		resolved, err := mc.Solar()
		if err != nil {
			return onError, "solar times unavailable: " + err.Error()
		}
		st = resolved
	}
	now := mc.Now
	if loc != nil {
		now = now.In(loc)
	}
	if !w.Contains(now, st) {
		return false, now.Format("15:04") + " outside hours " + w.String()
	}
	return true, ""
}

// entityMatches compares a Home Assistant entity's state, answering onError
// when the state can't be read.
func entityMatches(mc MatchContext, entityID, want string, onError bool) (bool, string) {
	if mc.EntityState == nil {
		return onError, "no home assistant client"
	}
	got, err := mc.EntityState(entityID)
	if err != nil {
		return onError, entityID + " unreadable: " + err.Error()
	}
	if !strings.EqualFold(got, want) {
		return false, fmt.Sprintf("%s is %q, want %q", entityID, got, want)
	}
	return true, ""
}

// excludesEveryone reports whether every person mc saw is a recognized,
// excluded face — an unaccounted-for person means it should not suppress.
func excludesEveryone(exclude []string, mc MatchContext) bool {
	excluded := 0
	for _, sl := range mc.SubLabels {
		if contains(exclude, sl) {
			excluded++
		}
	}
	if excluded == 0 {
		return false
	}
	people := 0
	for _, l := range mc.Labels {
		if l == "person" {
			people++
		}
	}
	return excluded >= people
}

func contains(haystack []string, needle string) bool {
	return slices.ContainsFunc(haystack, func(h string) bool { return strings.EqualFold(h, needle) })
}

// anyIn reports whether any of want appears in got.
func anyIn(want, got []string) bool {
	return slices.ContainsFunc(want, func(w string) bool { return contains(got, w) })
}
