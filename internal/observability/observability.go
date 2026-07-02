// Package observability wires ORBIT's logging, tracing, and metrics
// foundations (DESIGN §3): structured slog with trace correlation, an
// env-gated OTLP trace exporter, and a Prometheus registry. Every layer
// takes a *slog.Logger and a context; nothing logs through globals.
//
// Invariant: observability must never block or backpressure a data-plane
// or control-plane hot path. The rules that guarantee it:
//
//   - Per-packet / per-UE-at-scale volume is recorded with METRICS, not
//     logs. Prometheus counters/histograms are lock-free atomics; the OTLP
//     trace exporter is batched (WithBatcher) and drops rather than blocks
//     when its queue is full. Neither does synchronous I/O on the caller.
//   - Hot-path logs are emitted at Debug. slog checks Handler.Enabled and
//     returns before allocating the Record or touching the writer when the
//     configured level is higher, so a Debug call under the default (Info)
//     level costs one atomic compare — no formatting, no allocation, no
//     I/O. Callers must keep expensive work out of the log arguments
//     themselves (args evaluate before Enabled is consulted); guard with
//     log.Enabled(ctx, LevelDebug) when an argument is costly to compute.
//   - The default handler here writes synchronously to stderr, which is
//     appropriate only for low-frequency control-plane events (session
//     setup, procedure outcomes). Any sink that must accept high-frequency
//     records at an enabled level MUST be fronted by a bounded, non-blocking
//     handler that drops (and counts drops) rather than block the producer.
//     That async handler lands with the data plane (Phase 1b), where it has
//     a real producer to be tested against, not before.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// redactedKeys are attribute keys whose values never reach a log sink:
// subscriber credentials (Ki/OPc) transit the API from Phase 1a on and a
// leaked long-term key compromises the simulated subscriber permanently
// (DESIGN §8, credential handling).
var redactedKeys = map[string]bool{
	"k": true, "ki": true, "opc": true, "op": true, "key": true,
}

// NewLogger returns a JSON slog.Logger that redacts credential attributes
// and stamps trace/span ids from the context onto every record logged with
// the *Context variants.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if redactedKeys[strings.ToLower(a.Key)] {
				return slog.String(a.Key, "[redacted]")
			}
			return a
		},
	})
	return slog.New(traceHandler{h})
}

// traceHandler annotates records with the active span's trace_id/span_id so
// logs correlate to OTLP traces. This is the correlation half of the
// otelslog bridge; exporting logs themselves over OTLP can layer on later
// without changing call sites.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// SetupTracing installs an OTLP/gRPC trace exporter when endpoint is
// non-empty (typically from OTEL_EXPORTER_OTLP_ENDPOINT) and returns a
// shutdown func. With an empty endpoint tracing stays a no-op and the
// returned shutdown does nothing — unit-CI and headless runs need no
// collector.
func SetupTracing(ctx context.Context, endpoint, serviceName, version string) (func(context.Context) error, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// NewRegistry returns a Prometheus registry preloaded with the process and
// Go runtime collectors. Engine subsystems register their own metrics on it.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}
