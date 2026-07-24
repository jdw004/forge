// Package telemetry provides process-local Prometheus metrics and optional
// OTLP/HTTP traces. It never registers global collectors or tracer providers.
package telemetry

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"forge/internal/engine"
	"forge/internal/model"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type Config struct {
	ServiceName string
	Endpoint    string
	SampleRatio float64
}

type Telemetry struct {
	registry     *prometheus.Registry
	tracer       trace.Tracer
	shutdown     func(context.Context) error
	active       prometheus.Gauge
	capacity     prometheus.Gauge
	tokens       prometheus.Counter
	attempts     *prometheus.CounterVec
	completed    *prometheus.CounterVec
	breakers     *prometheus.CounterVec
	duration     prometheus.Histogram
	requests     *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// Local enables metrics only, with no exporter and no network calls.
func Local() *Telemetry {
	return newTelemetry(noop.NewTracerProvider(), func(context.Context) error { return nil })
}

func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	if math.IsNaN(cfg.SampleRatio) || cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, errors.New("trace sample ratio must be between 0 and 1")
	}
	if cfg.Endpoint == "" {
		return Local(), nil
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("OTLP endpoint must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "forge"
	}
	options := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(u.Host),
		otlptracehttp.WithURLPath(strings.TrimRight(u.Path, "/") + "/v1/traces"),
		otlptracehttp.WithTimeout(2 * time.Second),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	}
	if u.Scheme == "http" {
		options = append(options, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		// The default nonblocking queue drops telemetry on overload rather than
		// holding up task execution. Export and shutdown are bounded separately.
		sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(2048), sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(3*time.Second)),
	)
	return newTelemetry(provider, provider.Shutdown), nil
}

func newTelemetry(provider trace.TracerProvider, shutdown func(context.Context) error) *Telemetry {
	t := &Telemetry{
		registry: prometheus.NewRegistry(), tracer: provider.Tracer("forge"), shutdown: shutdown,
		active:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "forge_task_active", Help: "Task attempts executing or awaiting their completion acknowledgement in this process."}),
		capacity:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "forge_worker_capacity", Help: "Configured task slots in this process."}),
		tokens:       prometheus.NewCounter(prometheus.CounterOpts{Name: "forge_task_tokens_total", Help: "Simulated tokens consumed by all attempts, including abandoned attempts."}),
		attempts:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "forge_task_attempts_total", Help: "Finished execution attempts, including results not accepted by the store."}, []string{"state", "reason", "committed"}),
		completed:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "forge_tasks_completed_total", Help: "Task completions accepted by the store from this process."}, []string{"state"}),
		breakers:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "forge_breaker_trips_total", Help: "Token and runtime breaker trips observed by this process."}, []string{"reason"}),
		duration:     prometheus.NewHistogram(prometheus.HistogramOpts{Name: "forge_task_attempt_seconds", Help: "Attempt duration including completion acknowledgement.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 3, 5, 10, 30, 60, 300}}),
		requests:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "forge_http_requests_total", Help: "HTTP requests by method, route template, and status; excludes metric scrapes."}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "forge_http_request_seconds", Help: "HTTP request duration; excludes metric scrapes.", Buckets: prometheus.DefBuckets}, []string{"method", "route"}),
	}
	t.registry.MustRegister(t.active, t.capacity, t.tokens, t.attempts, t.completed, t.breakers, t.duration, t.requests, t.httpDuration, prometheus.NewGoCollector())
	return t
}

func (t *Telemetry) Shutdown(ctx context.Context) error { return t.shutdown(ctx) }
func (t *Telemetry) Handler() http.Handler {
	return promhttp.HandlerFor(t.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}
func (t *Telemetry) SetCapacity(slots int) { t.capacity.Set(float64(slots)) }

func reasonLabel(reason string) string {
	switch reason {
	case "":
		return "none"
	case engine.ReasonTokenLimit, engine.ReasonRuntimeLimit, engine.ReasonCancelled, engine.ReasonInvalidTask:
		return reason
	default:
		return "other"
	}
}

func stateLabel(state model.State) string {
	switch state {
	case model.Succeeded, model.Failed, model.Cancelled:
		return string(state)
	default:
		return "other"
	}
}

// StartTask implements worker.Observer. IDs belong in spans, never metric
// labels. An uncommitted result is an attempt, not a durable task completion.
func (t *Telemetry) StartTask(ctx context.Context, task model.Task) (context.Context, func(engine.Result, bool)) {
	started := time.Now()
	t.active.Inc()
	ctx, span := t.tracer.Start(ctx, "forge.task.execute", trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(
		attribute.String("forge.workflow.id", task.WorkflowID), attribute.String("forge.task.id", task.ID),
		attribute.String("forge.worker.id", task.WorkerID), attribute.Int("forge.task.attempt", task.Attempt),
	))
	var once sync.Once
	return ctx, func(result engine.Result, committed bool) {
		once.Do(func() {
			defer span.End()
			t.active.Dec()
			t.duration.Observe(time.Since(started).Seconds())
			t.tokens.Add(float64(result.Tokens))
			state, reason := stateLabel(result.State), reasonLabel(result.Reason)
			t.attempts.WithLabelValues(state, reason, strconv.FormatBool(committed)).Inc()
			if committed {
				t.completed.WithLabelValues(state).Inc()
			}
			if reason == engine.ReasonTokenLimit || reason == engine.ReasonRuntimeLimit {
				t.breakers.WithLabelValues(reason).Inc()
			}
			span.SetAttributes(attribute.String("forge.task.state", state), attribute.String("forge.task.reason", reason), attribute.Bool("forge.task.committed", committed), attribute.Int("forge.task.tokens", result.Tokens))
			if result.State == model.Failed {
				span.SetStatus(codes.Error, reason)
			}
		})
	}
}

// HTTPMiddleware belongs before the API's recovery middleware so it observes
// the final 500 response on a recovered panic. Only W3C trace context is read;
// credentials, query strings, request bodies, and baggage are not recorded.
func (t *Telemetry) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		method := r.Method
		switch method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		default:
			method = "OTHER"
		}
		started := time.Now()
		ctx := (propagation.TraceContext{}).Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := t.tracer.Start(ctx, "HTTP request", trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(wrapped, r.WithContext(ctx))
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		status := wrapped.Status()
		if status == 0 {
			status = http.StatusOK
		}
		t.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
		t.httpDuration.WithLabelValues(method, route).Observe(time.Since(started).Seconds())
		span.SetName(method + " " + route)
		span.SetAttributes(attribute.String("http.request.method", method), attribute.String("http.route", route), attribute.Int("http.response.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, "HTTP server error")
		}
	})
}
