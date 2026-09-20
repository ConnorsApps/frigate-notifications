package config

import (
	"strings"
	"time"
)

// Duration is a time.Duration written as a YAML string ("30s", "5m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	if s == "" {
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }
