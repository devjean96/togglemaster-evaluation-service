package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

type AppMetrics struct {
	evaluations      metric.Int64Counter
	evaluationTime   metric.Float64Histogram
	cacheAccesses    metric.Int64Counter
	dependencyErrors metric.Int64Counter
	sqsFailures      metric.Int64Counter
}

func setupTelemetry(ctx context.Context) (func(context.Context) error, error) {
	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = "evaluation-service"
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, err
	}

	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tracerProvider := trace.NewTracerProvider(
		trace.WithBatcher(traceExporter),
		trace.WithResource(res),
		trace.WithSampler(samplerFromEnv()),
	)

	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		_ = tracerProvider.Shutdown(ctx)
		return nil, err
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(30*time.Second))),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(shutdownCtx context.Context) error {
		return errors.Join(meterProvider.Shutdown(shutdownCtx), tracerProvider.Shutdown(shutdownCtx))
	}, nil
}

func samplerFromEnv() trace.Sampler {
	ratio := 1.0
	if value := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 && parsed <= 1 {
			ratio = parsed
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER"))) {
	case "always_off":
		return trace.NeverSample()
	case "traceidratio":
		return trace.TraceIDRatioBased(ratio)
	default:
		return trace.ParentBased(trace.TraceIDRatioBased(ratio))
	}
}

func newAppMetrics() *AppMetrics {
	meter := otel.Meter("togglemaster/evaluation-service")
	evaluations, _ := meter.Int64Counter("togglemaster.evaluations")
	evaluationTime, _ := meter.Float64Histogram("togglemaster.evaluation.duration", metric.WithUnit("ms"))
	cacheAccesses, _ := meter.Int64Counter("togglemaster.evaluation.cache_accesses")
	dependencyErrors, _ := meter.Int64Counter("togglemaster.evaluation.dependency_errors")
	sqsFailures, _ := meter.Int64Counter("togglemaster.evaluation.sqs_publish_failures")
	return &AppMetrics{
		evaluations:      evaluations,
		evaluationTime:   evaluationTime,
		cacheAccesses:    cacheAccesses,
		dependencyErrors: dependencyErrors,
		sqsFailures:      sqsFailures,
	}
}

func (a *App) recordEvaluation(ctx context.Context, result bool, duration time.Duration) {
	if a.Metrics == nil {
		return
	}
	a.Metrics.evaluations.Add(ctx, 1, metric.WithAttributes(attribute.Bool("feature_flag.result", result)))
	a.Metrics.evaluationTime.Record(ctx, float64(duration.Microseconds())/1000, metric.WithAttributes(attribute.Bool("feature_flag.result", result)))
}

func (a *App) recordCacheAccess(ctx context.Context, hit bool) {
	if a.Metrics != nil {
		a.Metrics.cacheAccesses.Add(ctx, 1, metric.WithAttributes(attribute.Bool("cache.hit", hit)))
	}
}

func (a *App) recordDependencyError(ctx context.Context, dependency string) {
	if a.Metrics != nil {
		a.Metrics.dependencyErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("server.address", dependency)))
	}
}

func (a *App) recordSQSPublishFailure(ctx context.Context) {
	if a.Metrics != nil {
		a.Metrics.sqsFailures.Add(ctx, 1)
	}
}
