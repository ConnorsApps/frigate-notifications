package homelog

import (
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type options struct {
	level  zerolog.Level
	format string
}

// Option configures Setup.
type Option func(*options)

// WithLevelStr parses level and sets the global log level. An empty string
// is a no-op. If level is set but fails to parse, a warning is logged and
// the level is left unchanged.
func WithLevelStr(level string) Option {
	return func(o *options) {
		if level == "" {
			return
		}

		l, err := zerolog.ParseLevel(level)
		if err != nil {
			log.Warn().Err(err).Msgf("invalid log level '%s'", level)
			return
		}

		o.level = l
	}
}

// Setup configures the global zerolog logger with a ConsoleWriter and RFC3339 timestamps.
// Defaults come from the LOG_LEVEL=trace/debug/info/warn/error and LOG_FORMAT=pretty/json
// environment variables; opts are applied afterward and take precedence.
func Setup(opts ...Option) {
	o := options{level: zerolog.InfoLevel}

	if s := os.Getenv("LOG_LEVEL"); s != "" {
		l, err := zerolog.ParseLevel(s)
		if err != nil {
			log.Warn().Err(err).Msgf("invalid log level '%s'", s)
		} else {
			o.level = l
		}
	}

	o.format = strings.ToLower(os.Getenv("LOG_FORMAT"))

	for _, opt := range opts {
		opt(&o)
	}

	switch o.format {
	case "", "pretty":
		log.Logger = log.Output(zerolog.ConsoleWriter{
			Out:        os.Stderr,
			TimeFormat: time.RFC3339,
		})
	case "json":
		// JSON is the default for zerolog
	}

	zerolog.SetGlobalLevel(o.level)
}
