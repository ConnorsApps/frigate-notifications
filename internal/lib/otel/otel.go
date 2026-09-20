// Package otel bootstraps OpenTelemetry tracing and metrics from the standard
// OTEL_EXPORTER_OTLP_* environment variables. A signal with no endpoint
// configured is left as the SDK's no-op provider.
package otel

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	opentelemetry_otel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdkTrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// signalConfigured reports whether an OTLP endpoint is set for the signal
// ("TRACES" or "METRICS"), either specifically or via the shared variable.
func signalConfigured(signal string) bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// Setup bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
// It sets the global tracer and meter providers, so callers can use otel.Tracer() directly.
func Setup(ctx context.Context, serviceName, version string) (
	shutdown func(context.Context) error, err error,
) {
	var shutdownFuncs []func(context.Context) error

	shutdown = func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			"",
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		handleErr(err)
		return
	}

	opentelemetry_otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if signalConfigured("TRACES") {
		tp, terr := newTraceProvider(ctx, res)
		if terr != nil {
			handleErr(terr)
			return
		}
		shutdownFuncs = append(shutdownFuncs, tp.Shutdown)
		opentelemetry_otel.SetTracerProvider(tp)
	}

	if signalConfigured("METRICS") {
		mp, merr := newMeterProvider(ctx, res)
		if merr != nil {
			handleErr(merr)
			return
		}
		shutdownFuncs = append(shutdownFuncs, mp.Shutdown)
		opentelemetry_otel.SetMeterProvider(mp)
	}

	return
}

// newTraceProvider picks the gRPC or HTTP exporter from
// OTEL_EXPORTER_OTLP_TRACES_PROTOCOL / OTEL_EXPORTER_OTLP_PROTOCOL; the
// exporters read their endpoint and headers from the environment themselves.
func newTraceProvider(ctx context.Context, res *resource.Resource) (*sdkTrace.TracerProvider, error) {
	proto := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if proto == "" {
		proto = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}

	var exporter *otlptrace.Exporter
	var err error
	if strings.HasPrefix(proto, "grpc") {
		exporter, err = otlptracegrpc.New(ctx)
	} else {
		exporter, err = otlptracehttp.New(ctx)
	}
	if err != nil {
		return nil, err
	}

	return sdkTrace.NewTracerProvider(
		sdkTrace.WithBatcher(exporter),
		sdkTrace.WithResource(res),
	), nil
}

func newMeterProvider(ctx context.Context, res *resource.Resource) (*metric.MeterProvider, error) {
	exporter, err := otlpmetrichttp.New(ctx,
		// Pushes only happen once per export interval (default 60s), so
		// there's no cost to a fresh connection each time — and it avoids
		// racing a keep-alive connection that the far end (or conntrack)
		// already closed, which otherwise surfaces as a POST-body EOF that
		// Go's http.Transport won't silently retry.
		otlpmetrichttp.WithHTTPClient(&http.Client{
			Transport: &http.Transport{DisableKeepAlives: true},
		}),
	)
	if err != nil {
		return nil, err
	}

	return metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(exporter)),
		metric.WithResource(res),
	), nil
}

// NewHTTPClient returns an *http.Client instrumented with OTel tracing.
// Span names are formatted as "METHOD host/path" for readability.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithSpanNameFormatter(
			func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Host + r.URL.Path })),
	}
}
