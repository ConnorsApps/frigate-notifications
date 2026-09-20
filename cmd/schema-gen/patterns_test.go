package main

import (
	"regexp"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The patterns must agree with the parsers the app and the API server use:
// a false reject would block a valid install.
func TestDurationPattern(t *testing.T) {
	re := regexp.MustCompile(durationPattern)
	for _, s := range []string{"", "0", "+0", "30s", "1h30m", "1.5h", "500ms", "5.s", ".5s", "1µs", "1μs", "1us", "1ns", "-5s", "+5s", "5", "s", "1d", "1 s", "0s", "00"} {
		_, err := time.ParseDuration(s)
		want := err == nil || s == ""
		if got := re.MatchString(s); got != want {
			t.Errorf("%q: pattern match = %v, ParseDuration ok = %v", s, got, want)
		}
	}
}

func TestQuantityPattern(t *testing.T) {
	re := regexp.MustCompile(quantityPattern)
	for _, s := range []string{"64Mi", "500m", "1.5", "5Gi", "1e3", "1E3", "1e-3", "0", "+1", "-1", ".5", "1k", "1K", "1Ki", "1ki", "1M", "1Mi", "big", "", "1 Gi", "1.Gi"} {
		_, err := resource.ParseQuantity(s)
		if got := re.MatchString(s); got != (err == nil) {
			t.Errorf("%q: pattern match = %v, ParseQuantity ok = %v", s, got, err == nil)
		}
	}
}
