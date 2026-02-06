package server_test

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"rlqs/internal/config"
	"rlqs/internal/engine"
	"rlqs/internal/server"
	"rlqs/internal/storage"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func startTestServer(t *testing.T) (*server.Server, string) {
	t.Helper()

	store := storage.NewMemoryStorage()
	eng := engine.New(config.EngineConfig{
		DefaultRPS:        100,
		ReportingInterval: config.Duration{Duration: 10 * time.Second},
	}, store)

	srv := server.New(eng, slog.Default())

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go srv.Serve(lis)
	t.Cleanup(func() { srv.GracefulStop() })

	return srv, lis.Addr().String()
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestServer_HealthCheck(t *testing.T) {
	_, addr := startTestServer(t)
	conn := dial(t, addr)

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", resp.GetStatus())
	}
}

func TestServer_HealthCheckServiceName(t *testing.T) {
	_, addr := startTestServer(t)
	conn := dial(t, addr)

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{
		Service: "envoy.service.rate_limit_quota.v3.RateLimitQuotaService",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", resp.GetStatus())
	}
}

func TestServer_StreamRateLimitQuotas(t *testing.T) {
	_, addr := startTestServer(t)
	conn := dial(t, addr)

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{"key": "value"},
				},
				NumRequestsAllowed: 100,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetBucketAction()) != 1 {
		t.Fatalf("expected 1 action, got %d", len(resp.GetBucketAction()))
	}
	if resp.GetBucketAction()[0].GetBucketId().GetBucket()["key"] != "value" {
		t.Fatal("bucket id mismatch")
	}
}
