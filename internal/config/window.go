package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SolarEvent names a sun.sun-derived endpoint. Empty means a wall-clock endpoint.
type SolarEvent string

const (
	SolarNone    SolarEvent = ""
	SolarSunrise SolarEvent = "sunrise"
	SolarSunset  SolarEvent = "sunset"
	SolarDawn    SolarEvent = "dawn"
	SolarDusk    SolarEvent = "dusk"
)

var solarEvents = map[string]SolarEvent{
	"sunrise": SolarSunrise,
	"sunset":  SolarSunset,
	"dawn":    SolarDawn,
	"dusk":    SolarDusk,
}

// SolarTimes carries today's solar events as durations since local midnight.
type SolarTimes struct {
	Sunrise, Sunset, Dawn, Dusk time.Duration
}

// Endpoint is one side of a Window: either a wall-clock time of day or a
// solar event with an optional offset ("dusk+30m", "06:00").
type Endpoint struct {
	Solar  SolarEvent
	Clock  time.Duration // since midnight; only meaningful when Solar == SolarNone
	Offset time.Duration // only meaningful when Solar != SolarNone
	raw    string
}

func (e Endpoint) String() string { return e.raw }

// IsSolar reports whether resolving this endpoint requires sun.sun.
func (e Endpoint) IsSolar() bool { return e.Solar != SolarNone }

var (
	clockRe = regexp.MustCompile(`^([0-9]{1,2}):([0-9]{2})$`)
	ampmRe  = regexp.MustCompile(`^([0-9]{1,2})(?::([0-9]{2}))?\s*([ap]m)$`)
	solarRe = regexp.MustCompile(`^([a-z]+)(?:([+-])([0-9]+[a-z].*))?$`)
)

// namedClocks are fixed wall-clock times spelled as a word rather than a
// solar event — they never need sun.sun and take no ±offset.
var namedClocks = map[string]time.Duration{
	"noon":     12 * time.Hour,
	"midnight": 0,
}

const unrecognizedTimeHelp = `want "HH:MM", a 12-hour time like "9am"/"11:30pm", "noon"/"midnight", or one of sunrise/sunset/dawn/dusk with an optional ±offset`

// ParseEndpoint accepts "HH:MM", a 12-hour clock ("9am", "11:30pm"),
// "noon"/"midnight", or "<solar>[±<duration>]".
func ParseEndpoint(s string) (Endpoint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Endpoint{}, fmt.Errorf("empty time endpoint")
	}

	if m := clockRe.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			return Endpoint{}, fmt.Errorf("invalid time of day %q", s)
		}
		return Endpoint{Clock: time.Duration(h)*time.Hour + time.Duration(min)*time.Minute, raw: s}, nil
	}

	if m := ampmRe.FindStringSubmatch(strings.ToLower(s)); m != nil {
		h, _ := strconv.Atoi(m[1])
		min := 0
		if m[2] != "" {
			min, _ = strconv.Atoi(m[2])
		}
		if h < 1 || h > 12 || min > 59 {
			return Endpoint{}, fmt.Errorf("invalid time of day %q", s)
		}
		switch {
		case m[3] == "pm" && h != 12:
			h += 12
		case m[3] == "am" && h == 12:
			h = 0
		}
		return Endpoint{Clock: time.Duration(h)*time.Hour + time.Duration(min)*time.Minute, raw: s}, nil
	}

	if d, ok := namedClocks[strings.ToLower(s)]; ok {
		return Endpoint{Clock: d, raw: s}, nil
	}

	m := solarRe.FindStringSubmatch(strings.ToLower(s))
	if m == nil || solarEvents[m[1]] == "" {
		return Endpoint{}, fmt.Errorf("unrecognized time %q: %s", s, unrecognizedTimeHelp)
	}

	e := Endpoint{Solar: solarEvents[m[1]], raw: s}
	if m[3] != "" {
		d, err := time.ParseDuration(m[3])
		if err != nil {
			return Endpoint{}, fmt.Errorf("invalid offset in %q: %w", s, err)
		}
		if m[2] == "-" {
			d = -d
		}
		e.Offset = d
	}
	return e, nil
}

// UnmarshalYAML lets an endpoint be written as a bare scalar. A 24-hour
// wall-clock endpoint must be quoted in YAML — bare 06:00 is a sexagesimal
// integer. 12-hour ("9am"), named ("noon", "midnight"), and solar forms are
// unambiguous and don't need quoting.
func (e *Endpoint) UnmarshalYAML(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	parsed, err := ParseEndpoint(s)
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// resolve returns the endpoint's time of day, normalized into [0, 24h).
func (e Endpoint) resolve(st SolarTimes) time.Duration {
	if !e.IsSolar() {
		return e.Clock
	}
	var base time.Duration
	switch e.Solar {
	case SolarSunrise:
		base = st.Sunrise
	case SolarSunset:
		base = st.Sunset
	case SolarDawn:
		base = st.Dawn
	case SolarDusk:
		base = st.Dusk
	}
	return normalizeDay(base + e.Offset)
}

func normalizeDay(d time.Duration) time.Duration {
	const day = 24 * time.Hour
	d %= day
	if d < 0 {
		d += day
	}
	return d
}

// Window is an inclusive time-of-day range that wraps midnight when To is
// earlier than From. Endpoints are independently wall-clock or solar.
type Window struct {
	From Endpoint `yaml:"from"`
	To   Endpoint `yaml:"to"`
}

// NeedsSolar reports whether evaluating this window requires sun.sun.
func (w Window) NeedsSolar() bool { return w.From.IsSolar() || w.To.IsSolar() }

// Contains reports whether now (in its own location) falls in the window.
func (w Window) Contains(now time.Time, st SolarTimes) bool {
	n := time.Duration(now.Hour())*time.Hour +
		time.Duration(now.Minute())*time.Minute +
		time.Duration(now.Second())*time.Second

	from, to := w.From.resolve(st), w.To.resolve(st)
	if from <= to {
		return n >= from && n <= to
	}
	// Wraps midnight.
	return n >= from || n <= to
}

func (w Window) String() string { return w.From.raw + " to " + w.To.raw }
