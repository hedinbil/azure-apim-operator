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

func InitTracer(ctx context.Context) func(context.Context) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "opentelemetry-collector.opentelemetry-collector.svc.cluster.local:4317"
		log.Println("⚠️  OTEL_EXPORTER_OTLP_ENDPOINT not set, using default:", endpoint)
	} else {
		log.Println("📡 Using OTLP endpoint from env:", endpoint)
	}

	conn, err := grpc.DialContext(
		ctx,
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("❌ Failed to create gRPC connection to OTLP endpoint: %v", err)
	}

	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		log.Fatalf("❌ Failed to create OTLP exporter: %v", err)
	}

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

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	log.Println("✅ Tracer configured for Datadog via OTLP gRPC")
	log.Printf("ℹ️  Traces will be sent to %s with service: 'azure-apim-operator', env: '%s', version: '%s'\n",
		endpoint, os.Getenv("DD_ENV"), os.Getenv("DD_VERSION"))

	return tp.Shutdown
}
