// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Defaults for Config.
const (
	// DefaultServiceName is the `service.name` resource attribute (plan §Observability).
	DefaultServiceName = "sre-agent"
	// DefaultMetricInterval is how often the periodic reader exports to the collector.
	DefaultMetricInterval = 30 * time.Second
	// ProtocolGRPC selects the OTLP/gRPC exporters.
	ProtocolGRPC = "grpc"
	// ProtocolHTTP selects the OTLP/HTTP (protobuf) exporters.
	ProtocolHTTP = "http/protobuf"
)

// Standard OTLP environment variables honoured when the corresponding Config field is empty.
const (
	envEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envProtocol = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envHeaders  = "OTEL_EXPORTER_OTLP_HEADERS"
	envService  = "OTEL_SERVICE_NAME"
)

// Config configures the tool's own OpenTelemetry pipeline (FR-051, research §15).
//
// The zero value is valid and useful: with no endpoint configured the binary runs with real
// instrumentation and no exporter, so `aisre serve` needs no collector to start. This is
// deliberate — self-observability must never be a precondition for the tool working.
type Config struct {
	// ServiceName defaults to DefaultServiceName.
	ServiceName string
	// ServiceVersion is the build version, reported as `service.version`.
	ServiceVersion string
	// Environment is optional and reported as `deployment.environment.name`.
	Environment string
	// Endpoint is the OTLP collector endpoint. Empty falls back to
	// OTEL_EXPORTER_OTLP_ENDPOINT; still empty means "export nothing".
	Endpoint string
	// Protocol is "grpc" or "http/protobuf". Empty falls back to
	// OTEL_EXPORTER_OTLP_PROTOCOL, then to "grpc" (the OTLP default for this binary).
	Protocol string
	// Insecure disables TLS. It applies only to endpoints given without a scheme; an
	// endpoint written as a URL carries its own scheme.
	Insecure bool
	// Headers are sent with every export, for collectors behind an auth proxy. Empty falls
	// back to OTEL_EXPORTER_OTLP_HEADERS (`k=v,k2=v2`).
	Headers map[string]string
	// MetricInterval defaults to DefaultMetricInterval.
	MetricInterval time.Duration
	// ResourceAttributes are merged on top of the derived resource.
	ResourceAttributes []attribute.KeyValue
	// Logger receives setup decisions and asynchronous exporter errors. Defaults to
	// slog.Default().
	Logger *slog.Logger
	// Disabled forces the no-exporter path even when an endpoint is configured. `--no-otel`.
	Disabled bool
}

// ShutdownFunc flushes and releases the telemetry pipeline. It is safe to call more than once.
type ShutdownFunc func(context.Context) error

// Setup installs the global tracer provider, meter provider and propagators, and returns the
// shutdown function that flushes them. Callers must defer the shutdown with a bounded context:
//
//	shutdown, err := telemetry.Setup(ctx, cfg)
//	if err != nil { return err }
//	defer func() {
//	    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	    defer cancel()
//	    _ = shutdown(ctx)
//	}()
//
// When no endpoint is configured (and in tests), Setup still installs real SDK providers with
// no exporter attached: spans are created and dropped, instruments record into no reader.
// Instrumented code then behaves identically whether or not a collector exists.
func Setup(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	logger := cfg.logger()

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	endpoint := cfg.endpoint()
	protocol, err := cfg.protocol()
	if err != nil {
		return nil, err
	}

	var shutdowns []func(context.Context) error

	traceOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	metricOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	if endpoint == "" || cfg.Disabled {
		logger.Info("opentelemetry export disabled; instrumentation still records",
			"reason", exportDisabledReason(cfg, endpoint))
	} else {
		spanExporter, err := newSpanExporter(ctx, cfg, protocol, endpoint)
		if err != nil {
			return nil, err
		}
		metricExporter, err := newMetricExporter(ctx, cfg, protocol, endpoint)
		if err != nil {
			// The span exporter is already live; do not leak its connection.
			_ = spanExporter.Shutdown(ctx)
			return nil, err
		}
		traceOpts = append(traceOpts, sdktrace.WithBatcher(spanExporter))
		metricOpts = append(metricOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(cfg.metricInterval())),
		))
		logger.Info("opentelemetry export enabled", "endpoint", endpoint, "protocol", protocol)
	}

	tracerProvider := sdktrace.NewTracerProvider(traceOpts...)
	meterProvider := sdkmetric.NewMeterProvider(metricOpts...)
	shutdowns = append(shutdowns, tracerProvider.Shutdown, meterProvider.Shutdown)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// Exporter failures are asynchronous; without a handler they vanish. A graph that cannot
	// report on itself must at least say so (Principle: self-observability).
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("opentelemetry error", "error", err.Error())
	}))

	// Shutdown runs once: a process can reach its exit path more than one way (signal
	// handler and defer), and flushing a provider twice is an error in the SDK.
	var (
		once    sync.Once
		onceErr error
	)
	return func(ctx context.Context) error {
		once.Do(func() {
			var errs []error
			for _, fn := range shutdowns {
				if err := fn(ctx); err != nil {
					errs = append(errs, err)
				}
			}
			onceErr = errors.Join(errs...)
		})
		return onceErr
	}, nil
}

func exportDisabledReason(cfg Config, endpoint string) string {
	if cfg.Disabled {
		return "explicitly disabled"
	}
	if endpoint == "" {
		return "no " + envEndpoint + " and no configured endpoint"
	}
	return "unknown"
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.serviceName())}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(cfg.Environment))
	}
	attrs = append(attrs, cfg.ResourceAttributes...)
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		resource.WithAttributes(attrs...),
	)
	if err != nil {
		// resource.New returns a usable resource alongside partial/schema errors. A
		// malformed OTEL_RESOURCE_ATTRIBUTES must degrade telemetry, never stop the graph.
		if res == nil {
			return nil, fmt.Errorf("telemetry: build resource: %w", err)
		}
		cfg.logger().Warn("partial telemetry resource", "error", err.Error())
	}
	return res, nil
}

func newSpanExporter(ctx context.Context, cfg Config, protocol, endpoint string) (sdktrace.SpanExporter, error) {
	headers := cfg.headers()
	switch protocol {
	case ProtocolGRPC:
		opts := []otlptracegrpc.Option{}
		if isURL(endpoint) {
			opts = append(opts, otlptracegrpc.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))
			if cfg.Insecure {
				opts = append(opts, otlptracegrpc.WithInsecure())
			}
		}
		if len(headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(headers))
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp/grpc trace exporter: %w", err)
		}
		return exp, nil
	default:
		opts := []otlptracehttp.Option{}
		if isURL(endpoint) {
			opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlptracehttp.WithEndpoint(endpoint))
			if cfg.Insecure {
				opts = append(opts, otlptracehttp.WithInsecure())
			}
		}
		if len(headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(headers))
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp/http trace exporter: %w", err)
		}
		return exp, nil
	}
}

func newMetricExporter(ctx context.Context, cfg Config, protocol, endpoint string) (sdkmetric.Exporter, error) {
	headers := cfg.headers()
	switch protocol {
	case ProtocolGRPC:
		opts := []otlpmetricgrpc.Option{}
		if isURL(endpoint) {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(endpoint))
			if cfg.Insecure {
				opts = append(opts, otlpmetricgrpc.WithInsecure())
			}
		}
		if len(headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(headers))
		}
		exp, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp/grpc metric exporter: %w", err)
		}
		return exp, nil
	default:
		opts := []otlpmetrichttp.Option{}
		if isURL(endpoint) {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlpmetrichttp.WithEndpoint(endpoint))
			if cfg.Insecure {
				opts = append(opts, otlpmetrichttp.WithInsecure())
			}
		}
		if len(headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(headers))
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp/http metric exporter: %w", err)
		}
		return exp, nil
	}
}

func isURL(endpoint string) bool { return strings.Contains(endpoint, "://") }

func (c Config) serviceName() string {
	if c.ServiceName != "" {
		return c.ServiceName
	}
	if fromEnv := strings.TrimSpace(os.Getenv(envService)); fromEnv != "" {
		return fromEnv
	}
	return DefaultServiceName
}

func (c Config) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return strings.TrimSpace(os.Getenv(envEndpoint))
}

func (c Config) protocol() (string, error) {
	raw := c.Protocol
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv(envProtocol))
	}
	switch strings.ToLower(raw) {
	case "", ProtocolGRPC:
		return ProtocolGRPC, nil
	case ProtocolHTTP, "http", "http/json":
		// http/json is accepted as an alias: the exporters speak protobuf over HTTP, which
		// every collector that accepts http/json also accepts.
		return ProtocolHTTP, nil
	default:
		return "", fmt.Errorf("telemetry: unsupported %s=%q (want grpc or http/protobuf)", envProtocol, raw)
	}
}

func (c Config) headers() map[string]string {
	if len(c.Headers) > 0 {
		return c.Headers
	}
	raw := strings.TrimSpace(os.Getenv(envHeaders))
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func (c Config) metricInterval() time.Duration {
	if c.MetricInterval > 0 {
		return c.MetricInterval
	}
	return DefaultMetricInterval
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}
