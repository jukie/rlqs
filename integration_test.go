package rlqs_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/quota"
	"github.com/jukie/rlqs/internal/server"
	"github.com/jukie/rlqs/internal/storage"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func setupServer(t *testing.T, rps uint64) (*server.Server, *storage.MemoryStorage, string) {
	t.Helper()

	store := storage.NewMemoryStorage()
	logger := zaptest.NewLogger(t)

	eng := quota.NewDefaultEngine(quota.DefaultEngineConfig{
		DefaultStrategy: &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_RequestsPerTimeUnit_{
				RequestsPerTimeUnit: &typev3.RateLimitStrategy_RequestsPerTimeUnit{
					RequestsPerTimeUnit: rps,
					TimeUnit:            typev3.RateLimitUnit_SECOND,
				},
			},
		},
		AssignmentTTL: 10 * time.Second,
	})

	srv := server.New(logger, store, eng, config.ServerConfig{}, nil)

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
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web-api"}}
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId:           bid,
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
	key := storage.BucketKeyFromProto(bid)
	scopedKey := storage.DomainScopedKey("test", key)
	state, ok := store.Get(scopedKey)
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
				BucketId:           bid,
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

	state, _ = store.Get(scopedKey)
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

func TestIntegration_DomainBinding(t *testing.T) {
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

	// First message binds to "test" domain.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"name": "b1"}},
				NumRequestsAllowed: 10,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Read the assignment response.
	_, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}

	// Second message with different domain should cause error.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "other-domain",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"name": "b2"}},
				NumRequestsAllowed: 5,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should receive an error on next recv.
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for domain mismatch")
	}
}

func TestIntegration_GracefulShutdown(t *testing.T) {
	store := storage.NewMemoryStorage()
	logger := zaptest.NewLogger(t)

	eng := quota.NewDefaultEngine(quota.DefaultEngineConfig{
		DefaultStrategy: &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_RequestsPerTimeUnit_{
				RequestsPerTimeUnit: &typev3.RateLimitStrategy_RequestsPerTimeUnit{
					RequestsPerTimeUnit: 100,
					TimeUnit:            typev3.RateLimitUnit_SECOND,
				},
			},
		},
		AssignmentTTL: 20 * time.Second,
	})

	srv := server.New(logger, store, eng, config.ServerConfig{}, nil)

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
