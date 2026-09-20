package rules

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

func window(t *testing.T, from, to string) *config.Window {
	t.Helper()
	f, err := config.ParseEndpoint(from)
	if err != nil {
		t.Fatal(err)
	}
	x, err := config.ParseEndpoint(to)
	if err != nil {
		t.Fatal(err)
	}
	return &config.Window{From: f, To: x}
}

func clock(t *testing.T, hhmm string) time.Time {
	t.Helper()
	parsed, err := time.Parse("15:04", hhmm)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// baseContext is a plain daytime person detection on the front porch.
func baseContext(t *testing.T) MatchContext {
	return MatchContext{
		Camera:   "front_porch",
		Labels:   []string{"person"},
		Severity: "alert",
		Phase:    PhaseNew,
		Now:      clock(t, "12:00"),
	}
}

func TestMatchFirstRuleWins(t *testing.T) {
	rs := []config.Rule{
		{Name: "specific", When: config.RuleConditions{Cameras: []string{"garage"}}},
		{Name: "general", When: config.RuleConditions{Labels: []string{"person"}}},
	}
	idx, trace := Match(rs, baseContext(t))
	if idx != 1 {
		t.Fatalf("matched index %d, want 1", idx)
	}
	if trace[0].Matched {
		t.Error("the camera-specific rule should not have matched")
	}
	if !strings.Contains(trace[0].Reason, "front_porch") {
		t.Errorf("trace should say why rule 0 was rejected, got %q", trace[0].Reason)
	}
}

func TestMatchConditions(t *testing.T) {
	tests := []struct {
		name    string
		cond    config.RuleConditions
		mutate  func(*MatchContext)
		matched bool
	}{
		{name: "empty conditions match anything", matched: true},
		{name: "camera in list", cond: config.RuleConditions{Cameras: []string{"front_porch"}}, matched: true},
		{name: "camera not in list", cond: config.RuleConditions{Cameras: []string{"garage"}}},
		{name: "label present", cond: config.RuleConditions{Labels: []string{"person", "dog"}}, matched: true},
		{name: "label absent", cond: config.RuleConditions{Labels: []string{"package"}}},
		{name: "severity matches", cond: config.RuleConditions{Severity: []string{"alert"}}, matched: true},
		{
			name: "severity rejects a detection",
			cond: config.RuleConditions{Severity: []string{"alert"}},
			mutate: func(mc *MatchContext) {
				mc.Severity = "detection"
			},
		},
		{
			name: "zone required and present",
			cond: config.RuleConditions{Zones: []string{"back_porch"}},
			mutate: func(mc *MatchContext) {
				mc.Zones = []string{"back_porch"}
			},
			matched: true,
		},
		{name: "zone required and absent", cond: config.RuleConditions{Zones: []string{"back_porch"}}},
		{
			name: "sub-label excluded",
			cond: config.RuleConditions{ExcludeSubLabels: []string{"alice"}},
			mutate: func(mc *MatchContext) {
				mc.SubLabels = []string{"alice"}
			},
		},
		{
			name:    "sub-label exclusion does not fire for a stranger",
			cond:    config.RuleConditions{ExcludeSubLabels: []string{"alice"}},
			matched: true,
		},
		{
			name: "sub-label exclusion is case-insensitive",
			cond: config.RuleConditions{ExcludeSubLabels: []string{"alice"}},
			mutate: func(mc *MatchContext) {
				mc.SubLabels = []string{"Alice"}
			},
		},
		{
			name: "sub-label exclusion does not fire alongside an unrecognized person",
			cond: config.RuleConditions{ExcludeSubLabels: []string{"alice"}},
			mutate: func(mc *MatchContext) {
				mc.SubLabels = []string{"alice"}
				mc.Labels = []string{"person", "person"}
			},
			matched: true,
		},
		{
			name: "sub-label exclusion does not fire alongside a different recognized person",
			cond: config.RuleConditions{ExcludeSubLabels: []string{"alice"}},
			mutate: func(mc *MatchContext) {
				mc.SubLabels = []string{"alice", "bob"}
				mc.Labels = []string{"person", "person"}
			},
			matched: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := baseContext(t)
			if tc.mutate != nil {
				tc.mutate(&mc)
			}
			idx, trace := Match([]config.Rule{{Name: "r", When: tc.cond}}, mc)
			if got := idx == 0; got != tc.matched {
				t.Errorf("matched = %v, want %v (trace: %s)", got, tc.matched, trace[0])
			}
		})
	}
}

func TestMatchHoursWindow(t *testing.T) {
	night := []config.Rule{{
		Name: "night",
		When: config.RuleConditions{Hours: window(t, "23:00", "05:59")},
	}}

	mc := baseContext(t)
	mc.Now = clock(t, "02:00")
	if idx, _ := Match(night, mc); idx != 0 {
		t.Error("02:00 should be inside the overnight window")
	}

	mc.Now = clock(t, "12:00")
	if idx, trace := Match(night, mc); idx != -1 {
		t.Errorf("noon should be outside the overnight window (trace: %s)", trace[0])
	}
}

func TestMatchSolarWindowUsesInjectedTimes(t *testing.T) {
	rs := []config.Rule{{
		Name: "night",
		When: config.RuleConditions{Hours: window(t, "dusk+30m", "dawn-30m")},
	}}

	winter := func() (config.SolarTimes, error) {
		return config.SolarTimes{
			Dawn: 6*time.Hour + 55*time.Minute,
			Dusk: 17*time.Hour + 5*time.Minute,
		}, nil
	}

	mc := baseContext(t)
	mc.Solar = winter
	mc.Now = clock(t, "18:00")
	if idx, trace := Match(rs, mc); idx != 0 {
		t.Errorf("18:00 in winter is after dusk and should match (trace: %s)", trace[0])
	}

	mc.Now = clock(t, "12:00")
	if idx, _ := Match(rs, mc); idx != -1 {
		t.Error("midday should never be inside a dusk-to-dawn window")
	}
}

// The load-bearing failure mode: a broken Home Assistant must never cost a
// notification. In `when` an unresolvable condition counts as satisfied.
func TestWhenFailsOpen(t *testing.T) {
	rs := []config.Rule{{
		Name: "night",
		When: config.RuleConditions{
			Hours:       window(t, "dusk", "dawn"),
			EntityState: map[string]string{"binary_sensor.armed": "on"},
		},
	}}

	mc := baseContext(t)
	mc.Now = clock(t, "12:00") // Would be outside the window if solar resolved.
	mc.Solar = func() (config.SolarTimes, error) { return config.SolarTimes{}, errors.New("hass down") }
	mc.EntityState = func(string) (string, error) { return "", errors.New("hass down") }

	if idx, trace := Match(rs, mc); idx != 0 {
		t.Errorf("an unreachable Home Assistant must not suppress a rule (trace: %s)", trace[0])
	}
}

// ...and in `unless` the same failure must count as unsatisfied, or a dead
// Home Assistant would silently mute every rule carrying an unless block.
func TestUnlessFailsOpen(t *testing.T) {
	rs := []config.Rule{{
		Name: "night",
		When: config.RuleConditions{Labels: []string{"person"}},
		Unless: []config.RuleConditions{
			{EntityState: map[string]string{"alarm_control_panel.home": "disarmed"}},
		},
	}}

	mc := baseContext(t)
	mc.EntityState = func(string) (string, error) { return "", errors.New("hass down") }

	idx, trace := Match(rs, mc)
	if idx != 0 {
		t.Errorf("an unreadable entity must not suppress the rule (trace: %s)", trace[0])
	}
}

func TestUnlessSuppresses(t *testing.T) {
	rs := []config.Rule{{
		Name: "night",
		When: config.RuleConditions{Labels: []string{"person"}},
		Unless: []config.RuleConditions{
			{EntityState: map[string]string{"alarm_control_panel.home": "disarmed"}},
			{EntityState: map[string]string{"input_boolean.guest_mode": "on"}},
		},
	}}

	states := map[string]string{
		"alarm_control_panel.home": "armed_away",
		"input_boolean.guest_mode": "off",
	}
	mc := baseContext(t)
	mc.EntityState = func(id string) (string, error) { return states[id], nil }

	if idx, _ := Match(rs, mc); idx != 0 {
		t.Fatal("rule should fire when neither unless entry holds")
	}

	// Entries are ORed: either one alone suppresses.
	states["alarm_control_panel.home"] = "disarmed"
	idx, trace := Match(rs, mc)
	if idx != -1 {
		t.Error("a disarmed alarm should suppress the rule")
	}
	if trace[0].Suppress != "unless[0]" {
		t.Errorf("trace should name the suppressing clause, got %q", trace[0].Suppress)
	}

	states["alarm_control_panel.home"] = "armed_away"
	states["input_boolean.guest_mode"] = "on"
	if idx, _ := Match(rs, mc); idx != -1 {
		t.Error("guest mode alone should suppress the rule")
	}
}

// A rule's own Location (resolved from its timezone override at config load)
// must be used to evaluate its hours window instead of whatever zone Now
// happens to already be in.
func TestMatchRuleLocationOverride(t *testing.T) {
	// 04:00 in mc.Now's own zone is 22:00 in UTC-6 — inside the window only
	// once localized to the rule's own Location.
	otherZone := time.FixedZone("UTC-6", -6*3600)
	rs := []config.Rule{{
		Name:     "late-in-other-zone",
		When:     config.RuleConditions{Hours: window(t, "22:00", "23:59")},
		Location: otherZone,
	}}

	mc := baseContext(t)
	mc.Now = clock(t, "04:00")

	if idx, trace := Match(rs, mc); idx != 0 {
		t.Errorf("04:00 should be 22:00 in the rule's UTC-6 zone, inside the window (trace: %s)", trace[0])
	}

	rs[0].Location = nil
	if idx, trace := Match(rs, mc); idx != -1 {
		t.Errorf("without a location override, 04:00 should be outside the 22:00-23:59 window (trace: %s)", trace[0])
	}
}

// A dwell rule can't be judged at "new" — and must not shadow a rule that
// should fire immediately.
func TestMinDwellDeferredAtNew(t *testing.T) {
	rs := []config.Rule{
		{Name: "loiter", When: config.RuleConditions{MinDwell: config.Duration(15 * time.Second)}},
		{Name: "immediate", When: config.RuleConditions{Labels: []string{"person"}}},
	}

	mc := baseContext(t)
	idx, trace := Match(rs, mc)
	if idx != 1 {
		t.Fatalf("matched %d, want the immediate rule at index 1", idx)
	}
	if !strings.Contains(trace[0].Reason, "deferred") {
		t.Error("the dwell rule should be marked deferred, not rejected")
	}

	// Once the object has dwelt, the higher-priority rule wins.
	mc.Phase = PhaseEnd
	mc.Dwell = 30 * time.Second
	if idx, _ := Match(rs, mc); idx != 0 {
		t.Error("the dwell rule should match at end once the dwell is satisfied")
	}

	// But not before.
	mc.Dwell = 5 * time.Second
	if idx, _ := Match(rs, mc); idx != 1 {
		t.Error("a short dwell should fall through to the immediate rule")
	}
}
