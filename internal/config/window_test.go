package config

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) Endpoint {
	t.Helper()
	e, err := ParseEndpoint(s)
	if err != nil {
		t.Fatalf("ParseEndpoint(%q): %v", s, err)
	}
	return e
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		in      string
		solar   SolarEvent
		clock   time.Duration
		offset  time.Duration
		wantErr bool
	}{
		{in: "06:00", clock: 6 * time.Hour},
		{in: "23:59", clock: 23*time.Hour + 59*time.Minute},
		{in: "0:05", clock: 5 * time.Minute},
		{in: "dusk", solar: SolarDusk},
		{in: "dawn-30m", solar: SolarDawn, offset: -30 * time.Minute},
		{in: "sunset+1h30m", solar: SolarSunset, offset: 90 * time.Minute},
		{in: "sunrise", solar: SolarSunrise},
		{in: "24:00", wantErr: true},
		{in: "06:60", wantErr: true},
		{in: "dusk+notaduration", wantErr: true},
		{in: "", wantErr: true},

		{in: "9am", clock: 9 * time.Hour},
		{in: "9AM", clock: 9 * time.Hour},
		{in: "11pm", clock: 23 * time.Hour},
		{in: "11PM", clock: 23 * time.Hour},
		{in: "9:30am", clock: 9*time.Hour + 30*time.Minute},
		{in: "9:30 pm", clock: 21*time.Hour + 30*time.Minute},
		{in: "12am", clock: 0}, // 12am is midnight, not 12:00
		{in: "12pm", clock: 12 * time.Hour},
		{in: "noon", clock: 12 * time.Hour},
		{in: "midnight", clock: 0},
		{in: "13pm", wantErr: true},
		{in: "0am", wantErr: true},
		{in: "9:60am", wantErr: true},
	}

	for _, tc := range tests {
		got, err := ParseEndpoint(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseEndpoint(%q) = %+v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEndpoint(%q): %v", tc.in, err)
			continue
		}
		if got.Solar != tc.solar || got.Clock != tc.clock || got.Offset != tc.offset {
			t.Errorf("ParseEndpoint(%q) = %+v, want solar=%q clock=%v offset=%v",
				tc.in, got, tc.solar, tc.clock, tc.offset)
		}
	}
}

// summer and winter stand in for the reason solar windows exist at all: the
// same wall-clock rule covers very different amounts of darkness.
var (
	summerSun = SolarTimes{
		Dawn: 5*time.Hour + 10*time.Minute, Sunrise: 5*time.Hour + 45*time.Minute,
		Sunset: 20*time.Hour + 30*time.Minute, Dusk: 21*time.Hour + 5*time.Minute,
	}
	winterSun = SolarTimes{
		Dawn: 6*time.Hour + 55*time.Minute, Sunrise: 7*time.Hour + 25*time.Minute,
		Sunset: 16*time.Hour + 35*time.Minute, Dusk: 17*time.Hour + 5*time.Minute,
	}
)

func at(t *testing.T, clock string) time.Time {
	t.Helper()
	parsed, err := time.Parse("15:04", clock)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestWindowContains(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		sun      SolarTimes
		clock    string
		want     bool
	}{
		{name: "wall clock inside", from: "06:00", to: "22:59", clock: "12:00", want: true},
		{name: "wall clock before", from: "06:00", to: "22:59", clock: "05:59", want: false},
		{name: "wall clock on lower bound", from: "06:00", to: "22:59", clock: "06:00", want: true},
		{name: "wall clock on upper bound", from: "06:00", to: "22:59", clock: "22:59", want: true},

		{name: "wraps midnight, late", from: "23:00", to: "05:59", clock: "23:30", want: true},
		{name: "wraps midnight, early", from: "23:00", to: "05:59", clock: "02:00", want: true},
		{name: "wraps midnight, outside", from: "23:00", to: "05:59", clock: "12:00", want: false},

		// The whole point: 18:00 in December is dark and should be covered by
		// the night rule, while the same clock time in June should not be.
		{name: "solar night in winter", from: "dusk+30m", to: "dawn-30m", sun: winterSun, clock: "18:00", want: true},
		{name: "solar night in summer", from: "dusk+30m", to: "dawn-30m", sun: summerSun, clock: "18:00", want: false},
		{name: "solar night both seasons at 3am", from: "dusk+30m", to: "dawn-30m", sun: summerSun, clock: "03:00", want: true},

		{name: "solar day in summer", from: "dawn-30m", to: "dusk+30m", sun: summerSun, clock: "18:00", want: true},
		{name: "solar day in winter", from: "dawn-30m", to: "dusk+30m", sun: winterSun, clock: "18:00", want: false},

		// A fixed 23:00 start would miss this; the solar one catches it.
		{name: "winter evening covered by solar but not 23:00", from: "dusk+30m", to: "dawn-30m", sun: winterSun, clock: "19:45", want: true},

		{name: "mixed wall clock and solar", from: "22:00", to: "dawn-30m", sun: winterSun, clock: "23:00", want: true},
		{name: "mixed, outside", from: "22:00", to: "dawn-30m", sun: winterSun, clock: "21:00", want: false},
		{name: "mixed, inside on the solar side", from: "22:00", to: "dawn-30m", sun: winterSun, clock: "06:00", want: true},

		// Offsets that cross midnight must wrap rather than go negative.
		{name: "offset wraps past midnight", from: "dusk+3h", to: "dawn", sun: winterSun, clock: "20:30", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := Window{From: mustParse(t, tc.from), To: mustParse(t, tc.to)}
			if got := w.Contains(at(t, tc.clock), tc.sun); got != tc.want {
				t.Errorf("Window(%s to %s).Contains(%s) = %v, want %v", tc.from, tc.to, tc.clock, got, tc.want)
			}
		})
	}
}

// A DST transition must not shift a wall-clock window: 23:30 local is inside
// a 23:00-05:59 window on the night the clocks change, same as any other.
func TestWindowAcrossDSTBoundary(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	w := Window{From: mustParse(t, "23:00"), To: mustParse(t, "05:59")}

	// 2026-03-08 is a US spring-forward date; 2026-11-01 is fall-back.
	for _, day := range []time.Time{
		time.Date(2026, 3, 8, 1, 30, 0, 0, loc),
		time.Date(2026, 11, 1, 1, 30, 0, 0, loc),
	} {
		if !w.Contains(day, SolarTimes{}) {
			t.Errorf("01:30 on %s should be inside the overnight window", day.Format("2006-01-02"))
		}
	}
}

func TestWindowNeedsSolar(t *testing.T) {
	wall := Window{From: mustParse(t, "06:00"), To: mustParse(t, "22:00")}
	if wall.NeedsSolar() {
		t.Error("wall-clock window should not need solar times")
	}
	mixed := Window{From: mustParse(t, "06:00"), To: mustParse(t, "dusk")}
	if !mixed.NeedsSolar() {
		t.Error("window with a solar endpoint should need solar times")
	}
}
