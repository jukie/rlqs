package server

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/jukie/rlqs/internal/storage"
)

// --- fakes ---

type fakeStore struct {
	usages  []storage.UsageReport
	removed []storage.BucketKey
}

func (f *fakeStore) RecordUsage(_ context.Context, _ string, r storage.UsageReport) error {
	f.usages = append(f.usages, r)
	return nil
}

func (f *fakeStore) RemoveBucket(_ context.Context, _ string, key storage.BucketKey) error {
	f.removed = append(f.removed, key)
	return nil
}

type fakeEngine struct {
	actions []*rlqspb.RateLimitQuotaResponse_BucketAction
}

func (f *fakeEngine) ProcessUsage(_ context.Context, _ string, _ []storage.UsageReport) ([]*rlqspb.RateLimitQuotaResponse_BucketAction, error) {
	return f.actions, nil
}

// --- helpers ---

type testEnv struct {
	store  *fakeStore
	engine *fakeEngine
	srv    *grpc.Server
	client rlqspb.RateLimitQuotaServiceClient
	conn   *grpc.ClientConn
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	logger := zaptest.NewLogger(t)

	st := &fakeStore{}
	eng := &fakeEngine{}

	srv := grpc.NewServer()
	Register(srv, logger, st, eng)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		t.Fatal(err)
	}

	return &testEnv{
		store:  st,
		engine: eng,
		srv:    srv,
		client: rlqspb.NewRateLimitQuotaServiceClient(conn),
		conn:   conn,
	}
}

func (e *testEnv) teardown() {
	e.conn.Close()
	e.srv.GracefulStop()
}

func bucketID(kvs ...string) *rlqspb.BucketId {
	m := make(map[string]string, len(kvs)/2)
	for i := 0; i < len(kvs)-1; i += 2 {
		m[kvs[i]] = kvs[i+1]
	}
	return &rlqspb.BucketId{Bucket: m}
}

func usageReport(bid *rlqspb.BucketId, allowed, denied uint64) *rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage {
	return &rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
		BucketId:           bid,
		TimeElapsed:        durationpb.New(5 * time.Second),
		NumRequestsAllowed: allowed,
		NumRequestsDenied:  denied,
	}
}

// --- tests ---

func TestDomainBindingFirstMessage(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "test")
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 100, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	stream.CloseSend()
	// Drain any responses.
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(env.store.usages) != 1 {
		t.Fatalf("expected 1 usage, got %d", len(env.store.usages))
	}
	if env.store.usages[0].NumRequestsAllowed != 100 {
		t.Fatalf("expected 100 allowed, got %d", env.store.usages[0].NumRequestsAllowed)
	}
}

func TestFirstMessageMissingDomain(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "test")
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for missing domain")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestDomainMismatch(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "test")

	// First message binds to "envoy".
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second message uses different domain.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "other-domain",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Recv should eventually return the domain mismatch error.
	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
		break
	}
}

func TestResponseSentWhenEngineReturnsActions(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	bid := bucketID("name", "test")
	env.engine.actions = []*rlqspb.RateLimitQuotaResponse_BucketAction{
		{
			BucketId: bid,
			BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
				QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
					AssignmentTimeToLive: durationpb.New(60 * time.Second),
				},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 50, 5)},
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
	action := resp.GetBucketAction()[0]
	if action.GetQuotaAssignmentAction() == nil {
		t.Fatal("expected quota assignment action")
	}
}

func TestCleanupOnDisconnect(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "test")
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	stream.CloseSend()
	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
	}

	// Give the server a moment to run cleanup.
	time.Sleep(100 * time.Millisecond)

	if len(env.store.removed) != 1 {
		t.Fatalf("expected 1 removed bucket, got %d", len(env.store.removed))
	}
}

func TestNoResponseWhenEngineReturnsNoActions(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	env.engine.actions = nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "test")
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Close and drain — should get EOF with no responses.
	stream.CloseSend()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("expected no responses")
	}
}
