package tracing

import (
	"context"
	"testing"

	"github.com/jukie/rlqs/internal/config"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestInitDisabled(t *testing.T) {
	shutdown, err := Init(context.Background(), config.TracingConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Global provider should remain the default noop when tracing is disabled.
	tp := otel.GetTracerProvider()
	if _, ok := tp.(*sdktrace.TracerProvider); ok {
		t.Fatal("expected noop provider when tracing disabled")
	}
}

func TestInitEnabled(t *testing.T) {
	// Use a non-routable endpoint so the exporter won't connect, but Init
	// should still succeed (the exporter connects lazily).
	shutdown, err := Init(context.Background(), config.TracingConfig{
		Enabled:  true,
		Endpoint: "127.0.0.1:0",
		Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	tp := otel.GetTracerProvider()
	if _, ok := tp.(*sdktrace.TracerProvider); !ok {
		t.Fatal("expected SDK TracerProvider when tracing enabled")
	}
}
