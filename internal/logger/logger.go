// Package logger provides OpenTelemetry tracing initialization for distributed tracing.
// This package configures the operator to send traces to an OpenTelemetry collector,
// which can then forward them to observability platforms like Datadog.
package logger

import (
	"context"
	"log"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// InitTracer initializes OpenTelemetry tracing for the operator.
// It sets up a gRPC connection to an OpenTelemetry collector and configures
// the global tracer provider with resource attributes (service name, environment, version).
//
// The function returns a shutdown function that should be called when the application
// exits to gracefully close the tracer provider.
//
// Environment variables:
//   - OTEL_EXPORTER_OTLP_ENDPOINT: The OTLP collector endpoint (defaults to local cluster service)
//   - DD_ENV: Datadog environment name (used as deployment environment)
//   - DD_VERSION: Service version (used for version tracking)
func InitTracer(ctx context.Context) func(context.Context) error {
	// Get the OTLP endpoint from environment or use default cluster-local service.
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "opentelemetry-collector.opentelemetry-collector.svc.cluster.local:4317"
		log.Println("⚠️  OTEL_EXPORTER_OTLP_ENDPOINT not set, using default:", endpoint)
	} else {
		log.Println("📡 Using OTLP endpoint from env:", endpoint)
	}

	// Create a gRPC connection to the OTLP collector.
	// Using insecure credentials is acceptable when the collector is in the same cluster.
	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("❌ Failed to create gRPC connection to OTLP endpoint: %v", err)
	}

	// Create an OTLP trace exporter that sends traces over gRPC.
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		log.Fatalf("❌ Failed to create OTLP exporter: %v", err)
	}

	// Create a resource with service metadata for trace attribution.
	// This helps identify traces from this operator in the observability platform.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("azure-apim-operator"),
			semconv.DeploymentEnvironmentKey.String(os.Getenv("DD_ENV")),
			semconv.ServiceVersionKey.String(os.Getenv("DD_VERSION")),
		),
	)
	if err != nil {
		log.Fatalf("❌ Failed to create resource: %v", err)
	}

	// Create a tracer provider with batched export and resource attributes.
	// Batching improves performance by sending multiple spans together.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// Set the global tracer provider so all tracing operations use this configuration.
	otel.SetTracerProvider(tp)

	log.Println("✅ Tracer configured for Datadog via OTLP gRPC")
	log.Printf("ℹ️  Traces will be sent to %s with service: 'azure-apim-operator', env: '%s', version: '%s'\n",
		endpoint, os.Getenv("DD_ENV"), os.Getenv("DD_VERSION"))

	return tp.Shutdown
}
