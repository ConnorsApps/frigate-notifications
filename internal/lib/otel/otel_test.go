package otel

import (
	"context"
	"testing"
)

func TestSignalConfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want map[string]bool
	}{
		{"unset", nil, map[string]bool{"TRACES": false, "METRICS": false}},
		{"base endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, map[string]bool{"TRACES": true, "METRICS": true}},
		{"traces only", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://c:4317"}, map[string]bool{"TRACES": true, "METRICS": false}},
		{"metrics only", map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://c/m"}, map[string]bool{"TRACES": false, "METRICS": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			for sig, want := range tc.want {
				if got := signalConfigured(sig); got != want {
					t.Errorf("signalConfigured(%s) = %v, want %v", sig, got, want)
				}
			}
		})
	}
}

// Exporters connect lazily, so Setup must succeed and shut down cleanly for
// both transports without a reachable collector.
func TestSetup(t *testing.T) {
	for _, proto := range []string{"", "grpc"} {
		t.Run("protocol="+proto, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/v1/metrics")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", proto)

			shutdown, err := Setup(context.Background(), "test", "v0")
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			_ = shutdown(context.Background())
		})
	}
}

func TestSetupDisabled(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		t.Setenv(k, "")
	}
	shutdown, err := Setup(context.Background(), "test", "v0")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
