package rlqs_test

import (
	"context"
	"io"
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

func setupServer(t *testing.T, rps uint64) (*server.Server, *storage.MemoryStorage, string) {
	t.Helper()

	store := storage.NewMemoryStorage()
	eng := engine.New(config.EngineConfig{
		DefaultRPS:        rps,
		ReportingInterval: config.Duration{Duration: 5 * time.Second},
	}, store)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(eng, logger)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go srv.Serve(lis)
	t.Cleanup(func() { srv.GracefulStop() })

	return srv, store, lis.Addr().String()
}

func TestIntegration_FullRoundTrip(t *testing.T) {
	_, store, addr := setupServer(t, 50)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Verify health check
	healthClient := healthpb.NewHealthClient(conn)
	hresp, err := healthClient.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if hresp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", hresp.GetStatus())
	}

	// Open RLQS stream
	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Send first report
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{"name": "web-api"},
				},
				NumRequestsAllowed: 42,
				NumRequestsDenied:  3,
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
		t.Fatalf("expected 1 bucket action, got %d", len(resp.GetBucketAction()))
	}

	action := resp.GetBucketAction()[0]
	if action.GetBucketId().GetBucket()["name"] != "web-api" {
		t.Fatalf("bucket id mismatch: %v", action.GetBucketId().GetBucket())
	}

	qa := action.GetQuotaAssignmentAction()
	if qa == nil {
		t.Fatal("expected quota assignment action")
	}
	if qa.GetRateLimitStrategy().GetRequestsPerTimeUnit().GetRequestsPerTimeUnit() != 50 {
		t.Fatalf("expected 50 rps assignment")
	}

	// Verify storage accumulated the report
	key := engine.BucketKeyFromID(action.GetBucketId())
	state, ok := store.Get(key)
	if !ok {
		t.Fatal("bucket not in storage")
	}
	if state.Allowed != 42 || state.Denied != 3 {
		t.Fatalf("unexpected storage state: %+v", state)
	}

	// Send second report for same bucket — storage should accumulate
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{"name": "web-api"},
				},
				NumRequestsAllowed: 8,
				NumRequestsDenied:  0,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}

	state, _ = store.Get(key)
	if state.Allowed != 50 || state.Denied != 3 {
		t.Fatalf("expected accumulated state (50,3), got: %+v", state)
	}
}

func TestIntegration_MultipleBucketsOneStream(t *testing.T) {
	_, _, addr := setupServer(t, 100)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "prod",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"ip": "10.0.0.1"}},
				NumRequestsAllowed: 20,
			},
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"ip": "10.0.0.2"}},
				NumRequestsAllowed: 30,
				NumRequestsDenied:  5,
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

	if len(resp.GetBucketAction()) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(resp.GetBucketAction()))
	}
}

func TestIntegration_GracefulShutdown(t *testing.T) {
	store := storage.NewMemoryStorage()
	eng := engine.New(config.EngineConfig{
		DefaultRPS:        100,
		ReportingInterval: config.Duration{Duration: 10 * time.Second},
	}, store)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(eng, logger)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(lis)
	}()

	// Connect and verify serving
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	healthClient := healthpb.NewHealthClient(conn)
	hresp, err := healthClient.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if hresp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", hresp.GetStatus())
	}

	// Trigger graceful shutdown
	srv.GracefulStop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server didn't stop within 5 seconds")
	}
}
