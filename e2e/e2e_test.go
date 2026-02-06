//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Environment addresses — overridable via env vars for CI.
var (
	envoyAddr = getEnv("E2E_ENVOY_ADDR", "localhost:10000")
	rlqsAddr  = getEnv("E2E_RLQS_ADDR", "localhost:18081")
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestMain brings up docker-compose before the suite and tears it down after.
func TestMain(m *testing.M) {
	if os.Getenv("E2E_SKIP_COMPOSE") == "1" {
		os.Exit(m.Run())
	}

	// Start compose stack.
	up := exec.Command("docker", "compose", "-f", "docker-compose.yaml", "up", "-d", "--build", "--wait")
	up.Dir = e2eDir()
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "docker compose up failed: %v\n", err)
		os.Exit(1)
	}

	// Wait for services to be reachable.
	if err := waitForTCP(rlqsAddr, 30*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "rlqs-server not reachable: %v\n", err)
		teardown()
		os.Exit(1)
	}
	if err := waitForHTTP("http://"+envoyAddr+"/allow", http.StatusOK, 30*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "envoy not reachable: %v\n", err)
		teardown()
		os.Exit(1)
	}

	code := m.Run()

	teardown()
	os.Exit(code)
}

func teardown() {
	down := exec.Command("docker", "compose", "-f", "docker-compose.yaml", "down", "-v", "--remove-orphans")
	down.Dir = e2eDir()
	down.Stdout = os.Stdout
	down.Stderr = os.Stderr
	_ = down.Run()
}

func e2eDir() string {
	if d := os.Getenv("E2E_DIR"); d != "" {
		return d
	}
	return "."
}

// ---------------------------------------------------------------------------
// Envoy-level tests: HTTP through Envoy → upstream, with RLQS on the side
// ---------------------------------------------------------------------------

// TestBasicRateLimiting verifies that the RLQS server's 5 RPS assignment
// eventually causes Envoy to return 429s.
func TestBasicRateLimiting(t *testing.T) {
	// The default bucket (catch_all) starts with ALLOW_ALL no-assignment
	// behavior, then the RLQS server assigns 5 RPS. We send a burst of
	// traffic and expect some 429s once the assignment takes effect.
	client := &http.Client{Timeout: 5 * time.Second}

	// Give Envoy time to receive at least one assignment from the RLQS server.
	// The reporting interval is 1s in the Envoy config, and the RLQS server
	// responds immediately with a 5 RPS assignment.
	require.Eventually(t, func() bool {
		return sendBurstAndCheck429(client, "http://"+envoyAddr+"/", 20)
	}, 15*time.Second, 500*time.Millisecond,
		"expected 429s after RLQS assignment of 5 RPS")
}

// TestNoAssignmentDenyAll verifies that the /deny path returns 429 before
// any RLQS assignment arrives (no_assignment_behavior = DENY_ALL).
func TestNoAssignmentDenyAll(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// The /deny bucket is configured with no_assignment_behavior DENY_ALL.
	// Even the very first request should be denied.
	resp, err := client.Get("http://" + envoyAddr + "/deny")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"expected 429 from DENY_ALL no-assignment behavior")
}

// TestNoAssignmentAllowAll verifies that the /allow path returns 200 before
// any RLQS assignment arrives (no_assignment_behavior = ALLOW_ALL).
func TestNoAssignmentAllowAll(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get("http://" + envoyAddr + "/allow")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"expected 200 from ALLOW_ALL no-assignment behavior")
}

// TestTTLExpiryFallback verifies that when the RLQS assignment expires,
// Envoy falls back to the expired_assignment_behavior (ALLOW_ALL for default).
func TestTTLExpiryFallback(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// First, ensure the assignment is active and enforcing 429s.
	require.Eventually(t, func() bool {
		return sendBurstAndCheck429(client, "http://"+envoyAddr+"/", 20)
	}, 15*time.Second, 500*time.Millisecond,
		"expected 429s from active RLQS assignment")

	// The RLQS server sets assignment TTL = 2 * reporting_interval = 4s.
	// The Envoy reporting interval is 1s. After the assignment expires,
	// expired_assignment_behavior = ALLOW_ALL should allow all traffic.
	// We simulate expiry by stopping the RLQS server.
	stopRLQS(t)

	// Wait for TTL expiry (assignment TTL is ~4s from the server config).
	time.Sleep(6 * time.Second)

	// After TTL expiry, the expired_assignment_behavior is ALLOW_ALL,
	// so all requests should succeed.
	var allOK bool
	for range 5 {
		resp, err := client.Get("http://" + envoyAddr + "/")
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			allOK = true
			break
		}
	}
	assert.True(t, allOK, "expected 200 after TTL expiry with ALLOW_ALL fallback")

	// Restart for subsequent tests.
	startRLQS(t)
}

// TestReconnection verifies that after the RLQS server restarts, Envoy
// reconnects and rate limiting resumes.
func TestReconnection(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Verify rate limiting is active.
	require.Eventually(t, func() bool {
		return sendBurstAndCheck429(client, "http://"+envoyAddr+"/", 20)
	}, 15*time.Second, 500*time.Millisecond,
		"expected 429s before restart")

	// Restart the RLQS server.
	stopRLQS(t)
	time.Sleep(2 * time.Second)
	startRLQS(t)

	// Wait for Envoy to reconnect and receive a new assignment.
	require.Eventually(t, func() bool {
		return sendBurstAndCheck429(client, "http://"+envoyAddr+"/", 20)
	}, 30*time.Second, 1*time.Second,
		"expected 429s to resume after RLQS reconnection")
}

// TestConcurrentClients verifies correct behavior under concurrent load.
func TestConcurrentClients(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Wait for assignment to take effect.
	require.Eventually(t, func() bool {
		return sendBurstAndCheck429(client, "http://"+envoyAddr+"/", 20)
	}, 15*time.Second, 500*time.Millisecond)

	var (
		total   atomic.Int64
		denied  atomic.Int64
		allowed atomic.Int64
		wg      sync.WaitGroup
	)

	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				resp, err := client.Get("http://" + envoyAddr + "/")
				if err != nil {
					continue
				}
				resp.Body.Close()
				total.Add(1)
				if resp.StatusCode == http.StatusTooManyRequests {
					denied.Add(1)
				} else if resp.StatusCode == http.StatusOK {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	t.Logf("concurrent burst: total=%d allowed=%d denied=%d",
		total.Load(), allowed.Load(), denied.Load())
	assert.Greater(t, denied.Load(), int64(0),
		"expected some 429s under concurrent load with 5 RPS limit")
}

// ---------------------------------------------------------------------------
// gRPC-level tests: direct protocol verification against the RLQS server
// ---------------------------------------------------------------------------

// TestGRPC_HealthCheck verifies the gRPC health check endpoint.
func TestGRPC_HealthCheck(t *testing.T) {
	conn := dialRLQS(t)

	hc := healthpb.NewHealthClient(conn)
	resp, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
}

// TestGRPC_RequestsPerTimeUnit verifies the server returns a
// RequestsPerTimeUnit strategy assignment.
func TestGRPC_RequestsPerTimeUnit(t *testing.T) {
	conn := dialRLQS(t)
	stream := openStream(t, conn)

	sendUsageReport(t, stream, "grpc-test", map[string]string{"name": "rptu-bucket"}, 10, 0)

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.GetBucketAction(), 1)

	action := resp.GetBucketAction()[0]
	qa := action.GetQuotaAssignmentAction()
	require.NotNil(t, qa, "expected quota assignment action")

	strategy := qa.GetRateLimitStrategy()
	require.NotNil(t, strategy)

	rptu := strategy.GetRequestsPerTimeUnit()
	require.NotNil(t, rptu, "expected RequestsPerTimeUnit strategy")
	assert.Equal(t, uint64(5), rptu.GetRequestsPerTimeUnit())
	assert.Equal(t, typepb.RateLimitUnit_SECOND, rptu.GetTimeUnit())
}

// TestGRPC_AssignmentTTL verifies that the server sets an assignment TTL.
func TestGRPC_AssignmentTTL(t *testing.T) {
	conn := dialRLQS(t)
	stream := openStream(t, conn)

	sendUsageReport(t, stream, "grpc-test", map[string]string{"name": "ttl-bucket"}, 1, 0)

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.GetBucketAction(), 1)

	qa := resp.GetBucketAction()[0].GetQuotaAssignmentAction()
	require.NotNil(t, qa)

	ttl := qa.GetAssignmentTimeToLive()
	require.NotNil(t, ttl, "expected assignment TTL to be set")
	assert.Greater(t, ttl.AsDuration(), time.Duration(0),
		"TTL should be positive")
}

// TestGRPC_MultipleBuckets verifies that the server handles multiple
// buckets in a single usage report.
func TestGRPC_MultipleBuckets(t *testing.T) {
	conn := dialRLQS(t)
	stream := openStream(t, conn)

	err := stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "multi-test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"svc": "alpha"}},
				NumRequestsAllowed: 5,
			},
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"svc": "beta"}},
				NumRequestsAllowed: 10,
				NumRequestsDenied:  2,
			},
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"svc": "gamma"}},
				NumRequestsAllowed: 1,
			},
		},
	})
	require.NoError(t, err)

	resp, err := stream.Recv()
	require.NoError(t, err)
	assert.Len(t, resp.GetBucketAction(), 3, "expected one action per bucket")
}

// TestGRPC_StreamPersistence verifies that the stream stays open across
// multiple report/response cycles.
func TestGRPC_StreamPersistence(t *testing.T) {
	conn := dialRLQS(t)
	stream := openStream(t, conn)

	for i := range 5 {
		sendUsageReport(t, stream,
			"persist-test",
			map[string]string{"name": fmt.Sprintf("bucket-%d", i)},
			uint64(i+1), 0)

		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, resp.GetBucketAction(), 1)
	}
}

// TestGRPC_AllStrategyTypes verifies the server can return quota assignments.
// The current server implementation returns RequestsPerTimeUnit for all
// buckets. This test confirms the assignment structure is well-formed and
// exercises the full gRPC round-trip for each bucket type scenario.
func TestGRPC_AllStrategyTypes(t *testing.T) {
	conn := dialRLQS(t)

	t.Run("RequestsPerTimeUnit", func(t *testing.T) {
		stream := openStream(t, conn)
		sendUsageReport(t, stream, "strategy-test", map[string]string{"type": "rptu"}, 100, 0)

		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, resp.GetBucketAction(), 1)

		qa := resp.GetBucketAction()[0].GetQuotaAssignmentAction()
		require.NotNil(t, qa)
		require.NotNil(t, qa.GetRateLimitStrategy())
		assert.NotNil(t, qa.GetRateLimitStrategy().GetRequestsPerTimeUnit(),
			"expected RequestsPerTimeUnit strategy")
	})

	t.Run("AssignmentStructure", func(t *testing.T) {
		stream := openStream(t, conn)
		sendUsageReport(t, stream, "strategy-test", map[string]string{"type": "structure"}, 50, 5)

		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, resp.GetBucketAction(), 1)

		action := resp.GetBucketAction()[0]
		assert.Equal(t, "structure", action.GetBucketId().GetBucket()["type"],
			"bucket ID should echo back the reported bucket")
		qa := action.GetQuotaAssignmentAction()
		require.NotNil(t, qa)
		assert.NotNil(t, qa.GetAssignmentTimeToLive(), "TTL must be set")
		assert.NotNil(t, qa.GetRateLimitStrategy(), "strategy must be set")
	})
}

// TestGRPC_BlanketAndTokenBucket verifies that a custom RLQS server
// returning BlanketRule and TokenBucket strategies produces well-formed
// responses. Runs an in-process mock RLQS server.
func TestGRPC_BlanketAndTokenBucket(t *testing.T) {
	// Start an in-process mock RLQS that returns specific strategies.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcSrv := grpc.NewServer()
	mock := &mockRLQSServer{}
	rlqspb.RegisterRateLimitQuotaServiceServer(grpcSrv, mock)
	go grpcSrv.Serve(lis)
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)

	t.Run("BlanketRule_DenyAll", func(t *testing.T) {
		mock.setStrategy(&typepb.RateLimitStrategy{
			Strategy: &typepb.RateLimitStrategy_BlanketRule_{
				BlanketRule: typepb.RateLimitStrategy_DENY_ALL,
			},
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := client.StreamRateLimitQuotas(ctx)
		require.NoError(t, err)

		sendUsageReport(t, stream, "mock", map[string]string{"name": "deny"}, 1, 0)
		resp, err := stream.Recv()
		require.NoError(t, err)

		strategy := resp.GetBucketAction()[0].GetQuotaAssignmentAction().GetRateLimitStrategy()
		assert.NotNil(t, strategy.GetBlanketRule(), "expected BlanketRule strategy")
		assert.Equal(t, typepb.RateLimitStrategy_DENY_ALL, strategy.GetBlanketRule())
	})

	t.Run("BlanketRule_AllowAll", func(t *testing.T) {
		mock.setStrategy(&typepb.RateLimitStrategy{
			Strategy: &typepb.RateLimitStrategy_BlanketRule_{
				BlanketRule: typepb.RateLimitStrategy_ALLOW_ALL,
			},
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := client.StreamRateLimitQuotas(ctx)
		require.NoError(t, err)

		sendUsageReport(t, stream, "mock", map[string]string{"name": "allow"}, 1, 0)
		resp, err := stream.Recv()
		require.NoError(t, err)

		strategy := resp.GetBucketAction()[0].GetQuotaAssignmentAction().GetRateLimitStrategy()
		assert.Equal(t, typepb.RateLimitStrategy_ALLOW_ALL, strategy.GetBlanketRule())
	})

	t.Run("TokenBucket", func(t *testing.T) {
		mock.setStrategy(&typepb.RateLimitStrategy{
			Strategy: &typepb.RateLimitStrategy_TokenBucket{
				TokenBucket: &typepb.TokenBucket{
					MaxTokens:    100,
					FillInterval: durationpb.New(time.Second),
				},
			},
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := client.StreamRateLimitQuotas(ctx)
		require.NoError(t, err)

		sendUsageReport(t, stream, "mock", map[string]string{"name": "token"}, 1, 0)
		resp, err := stream.Recv()
		require.NoError(t, err)

		tb := resp.GetBucketAction()[0].GetQuotaAssignmentAction().GetRateLimitStrategy().GetTokenBucket()
		require.NotNil(t, tb, "expected TokenBucket strategy")
		assert.Equal(t, uint32(100), tb.GetMaxTokens())
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func dialRLQS(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(rlqsAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func openStream(t *testing.T, conn *grpc.ClientConn) rlqspb.RateLimitQuotaService_StreamRateLimitQuotasClient {
	t.Helper()
	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	stream, err := client.StreamRateLimitQuotas(ctx)
	require.NoError(t, err)
	return stream
}

func sendUsageReport(
	t *testing.T,
	stream rlqspb.RateLimitQuotaService_StreamRateLimitQuotasClient,
	domain string,
	bucket map[string]string,
	allowed, denied uint64,
) {
	t.Helper()
	err := stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: domain,
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId:           &rlqspb.BucketId{Bucket: bucket},
				NumRequestsAllowed: allowed,
				NumRequestsDenied:  denied,
			},
		},
	})
	require.NoError(t, err)
}

// sendBurstAndCheck429 sends n requests and returns true if at least one 429
// was received.
func sendBurstAndCheck429(client *http.Client, url string, n int) bool {
	for range n {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			return true
		}
	}
	return false
}

func waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}

func waitForHTTP(url string, wantStatus int, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == wantStatus {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s to return %d", url, wantStatus)
}

func stopRLQS(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", "docker-compose.yaml", "stop", "rlqs-server")
	cmd.Dir = e2eDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("stop rlqs-server: %s (err: %v)", out, err)
	}
}

func startRLQS(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", "docker-compose.yaml", "start", "rlqs-server")
	cmd.Dir = e2eDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("start rlqs-server: %s (err: %v)", out, err)
	}
	// Wait for the server to come back.
	require.NoError(t, waitForTCP(rlqsAddr, 15*time.Second), "rlqs-server did not restart")
}

// mockRLQSServer is an in-process RLQS server that returns a configurable
// strategy for all bucket assignments.
type mockRLQSServer struct {
	rlqspb.UnimplementedRateLimitQuotaServiceServer

	mu       sync.Mutex
	strategy *typepb.RateLimitStrategy
}

func (m *mockRLQSServer) setStrategy(s *typepb.RateLimitStrategy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.strategy = s
}

func (m *mockRLQSServer) getStrategy() *typepb.RateLimitStrategy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.strategy
}

func (m *mockRLQSServer) StreamRateLimitQuotas(stream rlqspb.RateLimitQuotaService_StreamRateLimitQuotasServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		resp := &rlqspb.RateLimitQuotaResponse{}
		for _, usage := range req.GetBucketQuotaUsages() {
			resp.BucketAction = append(resp.BucketAction, &rlqspb.RateLimitQuotaResponse_BucketAction{
				BucketId: usage.GetBucketId(),
				BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
					QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
						AssignmentTimeToLive: durationpb.New(60 * time.Second),
						RateLimitStrategy:    m.getStrategy(),
					},
				},
			})
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}
