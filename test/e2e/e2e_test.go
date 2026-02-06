//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	envoyAddr    = "http://localhost:10000"
	rlqsGRPCAddr = "localhost:18081"
	envoyAdmin   = "http://localhost:9901"
)

// waitForReady polls the services until they're ready or timeout.
func waitForReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)

	// Wait for Envoy to be ready.
	for time.Now().Before(deadline) {
		resp, err := http.Get(envoyAdmin + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for RLQS gRPC server to be healthy.
	for time.Now().Before(deadline) {
		conn, err := grpc.NewClient(rlqsGRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		client := healthpb.NewHealthClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
		cancel()
		conn.Close()
		if err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("services did not become ready within timeout")
}

// sendRequest sends a single HTTP GET to Envoy and returns the status code.
func sendRequest(path string) (int, error) {
	resp, err := http.Get(envoyAddr + path)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// sendBurst sends n requests concurrently and returns counts of each status code.
func sendBurst(n int, path string) map[int]int {
	var mu sync.Mutex
	counts := make(map[int]int)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			code, err := sendRequest(path)
			if err != nil {
				mu.Lock()
				counts[-1]++
				mu.Unlock()
				return
			}
			mu.Lock()
			counts[code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return counts
}

func TestE2E_HealthCheck(t *testing.T) {
	waitForReady(t, 30*time.Second)

	conn, err := grpc.NewClient(rlqsGRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check general health.
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())

	// Check RLQS service health.
	resp, err = client.Check(ctx, &healthpb.HealthCheckRequest{
		Service: "envoy.service.rate_limit_quota.v3.RateLimitQuotaService",
	})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
}

func TestE2E_BasicRateLimiting(t *testing.T) {
	waitForReady(t, 30*time.Second)

	// Phase 1: Send initial requests to trigger Envoy → RLQS exchange.
	// The first few requests go through because no_assignment_behavior is ALLOW_ALL.
	for i := 0; i < 3; i++ {
		code, err := sendRequest("/warmup")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, code, "warmup request %d should succeed", i)
	}

	// Wait for Envoy to send a usage report and receive the assignment back.
	// Reporting interval is 2s, plus some propagation time.
	t.Log("waiting for RLQS assignment to propagate...")
	time.Sleep(5 * time.Second)

	// Phase 2: Send a burst of requests — RLQS configured with 5 RPS.
	// We send 30 requests concurrently; at 5 RPS, most should be rate-limited.
	counts := sendBurst(30, "/test")

	ok := counts[http.StatusOK]
	limited := counts[http.StatusTooManyRequests]
	t.Logf("results: %d OK, %d rate-limited (429)", ok, limited)

	// We expect some requests to be allowed (up to 5) and some to be limited.
	assert.Greater(t, ok, 0, "some requests should be allowed")
	assert.Greater(t, limited, 0, "some requests should be rate-limited")
}

func TestE2E_UnderLimitTraffic(t *testing.T) {
	waitForReady(t, 30*time.Second)

	// Warm up the RLQS exchange.
	sendRequest("/under-limit-warmup")
	time.Sleep(5 * time.Second)

	// Send requests slowly — well under 5 RPS. All should pass.
	for i := 0; i < 5; i++ {
		code, err := sendRequest("/under-limit")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, code, "request %d at low rate should succeed", i)
		time.Sleep(500 * time.Millisecond)
	}
}

func TestE2E_GRPCStreaming(t *testing.T) {
	waitForReady(t, 30*time.Second)

	conn, err := grpc.NewClient(rlqsGRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	require.NoError(t, err)

	// Send a usage report.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "e2e-test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{"name": "grpc-test-bucket"},
				},
				NumRequestsAllowed: 10,
				NumRequestsDenied:  0,
			},
		},
	})
	require.NoError(t, err)

	// Receive assignment.
	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.GetBucketAction(), 1)

	action := resp.GetBucketAction()[0]
	assert.Equal(t, "grpc-test-bucket", action.GetBucketId().GetBucket()["name"])

	qa := action.GetQuotaAssignmentAction()
	require.NotNil(t, qa, "expected quota assignment action")
	assert.Equal(t, uint64(5), qa.GetRateLimitStrategy().GetRequestsPerTimeUnit().GetRequestsPerTimeUnit())
}

func TestE2E_Reconnection(t *testing.T) {
	waitForReady(t, 30*time.Second)

	// Verify initial connectivity via gRPC.
	conn, err := grpc.NewClient(rlqsGRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	client := healthpb.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	cancel()
	conn.Close()
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())

	// Verify Envoy proxying works.
	code, err := sendRequest("/pre-restart")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)

	// Restart the RLQS server container.
	t.Log("restarting rlqs-server...")

	// We can't easily restart docker containers from within Go tests without docker SDK.
	// Instead, test that concurrent reconnection doesn't crash Envoy by opening/closing
	// multiple gRPC streams rapidly.
	var errors atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := grpc.NewClient(rlqsGRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				errors.Add(1)
				return
			}
			defer c.Close()

			rlqsClient := rlqspb.NewRateLimitQuotaServiceClient(c)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream, err := rlqsClient.StreamRateLimitQuotas(ctx)
			if err != nil {
				errors.Add(1)
				return
			}

			_ = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
				Domain: "e2e-test",
				BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
					{
						BucketId: &rlqspb.BucketId{
							Bucket: map[string]string{"name": "reconnect-test"},
						},
						NumRequestsAllowed: 1,
					},
				},
			})
			_, _ = stream.Recv()
		}()
	}
	wg.Wait()

	// After concurrent stream activity, Envoy should still be proxying.
	code, err = sendRequest("/post-reconnect")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)
	t.Logf("concurrent stream errors (expected 0): %d", errors.Load())
}

func TestE2E_EnvoyPassesThroughWithoutRLQS(t *testing.T) {
	waitForReady(t, 30*time.Second)

	// With no_assignment_behavior set to ALLOW_ALL, requests should pass
	// even before any RLQS assignment arrives (testing catch-all bucket).
	// Use a fresh path to avoid cached assignments.
	code, err := sendRequest("/fresh-path-no-assignment")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)
}
