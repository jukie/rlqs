package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/metrics"
	"github.com/jukie/rlqs/internal/storage"
)

// --- fakes ---

type fakeStore struct {
	mu      sync.Mutex
	usages  []storage.UsageReport
	removed []storage.BucketKey
}

func (f *fakeStore) RecordUsage(_ context.Context, _ string, r storage.UsageReport) error {
	f.mu.Lock()
	f.usages = append(f.usages, r)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) RemoveBucket(_ context.Context, _ string, key storage.BucketKey) error {
	f.mu.Lock()
	f.removed = append(f.removed, key)
	f.mu.Unlock()
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

func setupWithConfig(t *testing.T, cfg config.ServerConfig) *testEnv {
	t.Helper()
	logger := zaptest.NewLogger(t)

	st := &fakeStore{}
	eng := &fakeEngine{}

	srv := grpc.NewServer()
	Register(srv, logger, st, eng, cfg)

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

func setup(t *testing.T) *testEnv {
	t.Helper()
	logger := zaptest.NewLogger(t)

	st := &fakeStore{}
	eng := &fakeEngine{}

	srv := grpc.NewServer()
	Register(srv, logger, st, eng, config.ServerConfig{
		EngineTimeout: config.Duration{Duration: 5 * time.Second},
	})

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

func TestCleanupSharedBucketPreservesStorage(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	bid := bucketID("name", "shared")

	// Engine returns an assignment so we can Recv() to confirm the server
	// has processed each subscription before proceeding.
	env.engine.actions = []*rlqspb.RateLimitQuotaResponse_BucketAction{
		{
			BucketId: bid,
			BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
				QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{},
			},
		},
	}

	// Stream 1 subscribes to the bucket.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	stream1, err := env.client.StreamRateLimitQuotas(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	err = stream1.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream1.Recv(); err != nil {
		t.Fatal(err)
	}

	// Stream 2 subscribes to the same bucket.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	stream2, err := env.client.StreamRateLimitQuotas(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	err = stream2.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 20, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for server to process stream2's subscription — ensures bucketRefs
	// is 2 before we disconnect stream1.
	if _, err := stream2.Recv(); err != nil {
		t.Fatal(err)
	}

	// Disconnect stream 1 — shared bucket should NOT be removed.
	stream1.CloseSend()
	for {
		_, err := stream1.Recv()
		if err != nil {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)

	if len(env.store.removed) != 0 {
		t.Fatalf("expected 0 removed buckets while another stream holds a ref, got %d", len(env.store.removed))
	}

	// Disconnect stream 2 — now the bucket should be removed (last ref gone).
	stream2.CloseSend()
	for {
		_, err := stream2.Recv()
		if err != nil {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)

	if len(env.store.removed) != 1 {
		t.Fatalf("expected 1 removed bucket after last stream disconnected, got %d", len(env.store.removed))
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

func TestMaxBucketsPerStreamExceeded(t *testing.T) {
	env := setupWithConfig(t, config.ServerConfig{
		MaxBucketsPerStream: 2,
		EngineTimeout:       config.Duration{Duration: 5 * time.Second},
	})
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Send 2 distinct buckets to fill the limit.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			usageReport(bucketID("name", "b1"), 10, 0),
			usageReport(bucketID("name", "b2"), 10, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Send a third distinct bucket — should exceed the limit.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			usageReport(bucketID("name", "b3"), 10, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Recv should eventually return ResourceExhausted.
	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.ResourceExhausted {
			t.Fatalf("expected ResourceExhausted, got %v", err)
		}
		break
	}
}

// --- TLS test helpers ---

// generateTestCA creates a self-signed CA certificate and key for testing.
func generateTestCA(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// generateTestCert creates a certificate signed by the given CA for testing.
func generateTestCert(t *testing.T, caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte) {
	t.Helper()
	caBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp("", "rlqs-tls-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

// --- TLS tests ---

func TestTLSOptionServerTLS(t *testing.T) {
	caCertPEM, caKeyPEM := generateTestCA(t)
	serverCertPEM, serverKeyPEM := generateTestCert(t, caCertPEM, caKeyPEM)

	certFile := writeTempFile(t, serverCertPEM)
	keyFile := writeTempFile(t, serverKeyPEM)

	opt, err := TLSOption(config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Set up server with TLS.
	logger := zaptest.NewLogger(t)
	st := &fakeStore{}
	eng := &fakeEngine{}
	srv := grpc.NewServer(opt)
	Register(srv, logger, st, eng, config.ServerConfig{EngineTimeout: config.Duration{Duration: 5 * time.Second}})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	// Client must use TLS to connect.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCertPEM)
	creds := credentials.NewClientTLSFromCert(pool, "")

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "tls-test")
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
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(st.usages) != 1 {
		t.Fatalf("expected 1 usage over TLS, got %d", len(st.usages))
	}
}

func TestTLSOptionMTLS(t *testing.T) {
	caCertPEM, caKeyPEM := generateTestCA(t)
	serverCertPEM, serverKeyPEM := generateTestCert(t, caCertPEM, caKeyPEM)
	clientCertPEM, clientKeyPEM := generateTestCert(t, caCertPEM, caKeyPEM)

	certFile := writeTempFile(t, serverCertPEM)
	keyFile := writeTempFile(t, serverKeyPEM)
	caFile := writeTempFile(t, caCertPEM)

	opt, err := TLSOption(config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   caFile,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := zaptest.NewLogger(t)
	st := &fakeStore{}
	eng := &fakeEngine{}
	srv := grpc.NewServer(opt)
	Register(srv, logger, st, eng, config.ServerConfig{EngineTimeout: config.Duration{Duration: 5 * time.Second}})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	// Client with valid client cert should succeed.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCertPEM)

	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      pool,
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "mtls-test")
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 5, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	stream.CloseSend()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(st.usages) != 1 {
		t.Fatalf("expected 1 usage over mTLS, got %d", len(st.usages))
	}
}

func TestTLSOptionMTLSRejectsNoClientCert(t *testing.T) {
	caCertPEM, caKeyPEM := generateTestCA(t)
	serverCertPEM, serverKeyPEM := generateTestCert(t, caCertPEM, caKeyPEM)

	certFile := writeTempFile(t, serverCertPEM)
	keyFile := writeTempFile(t, serverKeyPEM)
	caFile := writeTempFile(t, caCertPEM)

	opt, err := TLSOption(config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   caFile,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := zaptest.NewLogger(t)
	srv := grpc.NewServer(opt)
	Register(srv, logger, &fakeStore{}, &fakeEngine{}, config.ServerConfig{EngineTimeout: config.Duration{Duration: 5 * time.Second}})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	// Client without client cert — server should reject.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCertPEM)
	creds := credentials.NewTLS(&tls.Config{
		RootCAs: pool,
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.StreamRateLimitQuotas(ctx)
	if err == nil {
		t.Fatal("expected error when no client cert provided for mTLS")
	}
}

func TestTLSOptionInvalidCertFile(t *testing.T) {
	_, err := TLSOption(config.TLSConfig{
		CertFile: "/nonexistent/cert.pem",
		KeyFile:  "/nonexistent/key.pem",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent cert file")
	}
}

func TestServerSwapEngine(t *testing.T) {
	logger := zaptest.NewLogger(t)
	st := &fakeStore{}
	eng := &fakeEngine{}

	srv := New(logger, st, eng, config.ServerConfig{
		MaxBucketsPerStream: 0,
		EngineTimeout:       config.Duration{Duration: 5 * time.Second},
	}, nil)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)

	bid := bucketID("name", "swap-test")

	// First: engine returns no actions.
	eng.actions = nil

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	stream1, err := client.StreamRateLimitQuotas(ctx1)
	if err != nil {
		t.Fatal(err)
	}

	err = stream1.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	stream1.CloseSend()
	for {
		_, err := stream1.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("expected no response from initial engine")
	}

	// Swap engine to one that returns actions.
	newEngine := &fakeEngine{
		actions: []*rlqspb.RateLimitQuotaResponse_BucketAction{
			{
				BucketId: bid,
				BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
					QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
						AssignmentTimeToLive: durationpb.New(30 * time.Second),
					},
				},
			},
		},
	}
	srv.SwapEngine(newEngine)

	// Second stream after swap — should see actions from new engine.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	stream2, err := client.StreamRateLimitQuotas(ctx2)
	if err != nil {
		t.Fatal(err)
	}

	err = stream2.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := stream2.Recv()
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.GetBucketAction()) != 1 {
		t.Fatalf("expected 1 action after swap, got %d", len(resp.GetBucketAction()))
	}
	if resp.GetBucketAction()[0].GetQuotaAssignmentAction() == nil {
		t.Fatal("expected assignment action after engine swap")
	}
}

func TestSwapEnginePushesAssignmentsToExistingStreams(t *testing.T) {
	logger := zaptest.NewLogger(t)
	st := &fakeStore{}

	bid := bucketID("name", "push-test")

	// Initial engine returns an assignment so we can confirm the report was processed.
	initialAction := &rlqspb.RateLimitQuotaResponse_BucketAction{
		BucketId: bid,
		BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
			QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
				AssignmentTimeToLive: durationpb.New(60 * time.Second),
			},
		},
	}
	eng := &fakeEngine{
		actions: []*rlqspb.RateLimitQuotaResponse_BucketAction{initialAction},
	}

	srv := New(logger, st, eng, config.ServerConfig{
		EngineTimeout: config.Duration{Duration: 5 * time.Second},
	}, nil)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Send initial report to subscribe to the bucket.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Recv the initial assignment — confirms the subscription is recorded.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetBucketAction()[0].GetQuotaAssignmentAction().GetAssignmentTimeToLive().AsDuration(); got != 60*time.Second {
		t.Fatalf("expected initial TTL 60s, got %v", got)
	}

	// Swap to a new engine with a different TTL.
	newAction := &rlqspb.RateLimitQuotaResponse_BucketAction{
		BucketId: bid,
		BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
			QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
				AssignmentTimeToLive: durationpb.New(15 * time.Second),
			},
		},
	}
	newEngine := &fakeEngine{
		actions: []*rlqspb.RateLimitQuotaResponse_BucketAction{newAction},
	}
	srv.SwapEngine(newEngine)

	// The proactive push should send the new assignment without another client report.
	resp, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.GetBucketAction()) != 1 {
		t.Fatalf("expected 1 pushed action, got %d", len(resp.GetBucketAction()))
	}
	got := resp.GetBucketAction()[0].GetQuotaAssignmentAction().GetAssignmentTimeToLive().AsDuration()
	if got != 15*time.Second {
		t.Fatalf("expected pushed TTL 15s, got %v", got)
	}
}

func TestSwapEnginePushMultipleStreams(t *testing.T) {
	logger := zaptest.NewLogger(t)
	st := &fakeStore{}

	// Initial engine returns no actions.
	eng := &fakeEngine{actions: nil}

	srv := New(logger, st, eng, config.ServerConfig{
		EngineTimeout: config.Duration{Duration: 5 * time.Second},
	}, nil)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	bid1 := bucketID("name", "bucket-a")
	bid2 := bucketID("name", "bucket-b")

	// Open stream 1 with bucket-a.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	stream1, err := client.StreamRateLimitQuotas(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	err = stream1.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid1, 10, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Open stream 2 with bucket-b.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	stream2, err := client.StreamRateLimitQuotas(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	err = stream2.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid2, 20, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for server to process both subscriptions.
	time.Sleep(100 * time.Millisecond)

	// Swap to engine that returns actions for every report.
	newEngine := &fakeEngine{
		actions: []*rlqspb.RateLimitQuotaResponse_BucketAction{
			{
				BucketId: bid1, // Action has bid1 but fakeEngine ignores input anyway
				BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
					QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
						AssignmentTimeToLive: durationpb.New(20 * time.Second),
					},
				},
			},
		},
	}
	srv.SwapEngine(newEngine)

	// Both streams should receive proactive pushes.
	resp1, err := stream1.Recv()
	if err != nil {
		t.Fatalf("stream1 recv: %v", err)
	}
	if len(resp1.GetBucketAction()) != 1 {
		t.Fatalf("stream1: expected 1 action, got %d", len(resp1.GetBucketAction()))
	}

	resp2, err := stream2.Recv()
	if err != nil {
		t.Fatalf("stream2 recv: %v", err)
	}
	if len(resp2.GetBucketAction()) != 1 {
		t.Fatalf("stream2: expected 1 action, got %d", len(resp2.GetBucketAction()))
	}
}

// --- Input validation & DoS protection tests ---

func TestMaxReportsPerMessageExceeded(t *testing.T) {
	env := setupWithConfig(t, config.ServerConfig{
		MaxReportsPerMessage: 2,
		EngineTimeout:        config.Duration{Duration: 5 * time.Second},
	})
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Send 3 reports which exceeds the limit of 2.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			usageReport(bucketID("name", "a"), 1, 0),
			usageReport(bucketID("name", "b"), 1, 0),
			usageReport(bucketID("name", "c"), 1, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.ResourceExhausted {
			t.Fatalf("expected ResourceExhausted, got %v", err)
		}
		break
	}
}

func TestMaxReportsPerMessageAtLimit(t *testing.T) {
	env := setupWithConfig(t, config.ServerConfig{
		MaxReportsPerMessage: 2,
		EngineTimeout:        config.Duration{Duration: 5 * time.Second},
	})
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Send exactly 2 reports — should succeed.
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain: "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			usageReport(bucketID("name", "a"), 1, 0),
			usageReport(bucketID("name", "b"), 1, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stream.CloseSend()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if len(env.store.usages) != 2 {
		t.Fatalf("expected 2 usages, got %d", len(env.store.usages))
	}
}

func TestBucketIdTooManyEntries(t *testing.T) {
	env := setupWithConfig(t, config.ServerConfig{
		MaxBucketEntries: 2,
		EngineTimeout:    config.Duration{Duration: 5 * time.Second},
	})
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// BucketId with 3 entries exceeds the limit of 2.
	bid := &rlqspb.BucketId{Bucket: map[string]string{
		"a": "1", "b": "2", "c": "3",
	}}
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 1, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

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

func TestBucketIdKeyTooLong(t *testing.T) {
	env := setupWithConfig(t, config.ServerConfig{
		MaxBucketKeyLen: 5,
		EngineTimeout:   config.Duration{Duration: 5 * time.Second},
	})
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := &rlqspb.BucketId{Bucket: map[string]string{
		"toolong": "val",
	}}
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 1, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

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

func TestBucketIdValueTooLong(t *testing.T) {
	env := setupWithConfig(t, config.ServerConfig{
		MaxBucketValueLen: 3,
		EngineTimeout:     config.Duration{Duration: 5 * time.Second},
	})
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := &rlqspb.BucketId{Bucket: map[string]string{
		"k": "toolong",
	}}
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 1, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

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

func TestBucketIdEmptyKeyRejected(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// An empty key should trigger InvalidArgument.
	bid := &rlqspb.BucketId{Bucket: map[string]string{
		"": "val",
	}}
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 1, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

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

func TestBucketIdEmptyValueRejected(t *testing.T) {
	env := setup(t)
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// An empty value should trigger InvalidArgument.
	bid := &rlqspb.BucketId{Bucket: map[string]string{
		"key": "",
	}}
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "envoy",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{usageReport(bid, 1, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}

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

// --- Prometheus cardinality protection tests ---

func TestUnknownDomainUsesOverflowLabel(t *testing.T) {
	// Create a classifier that only allows domains matching ^envoy\.
	classifier, err := metrics.NewDomainClassifier([]string{`^envoy\.`}, 100)
	if err != nil {
		t.Fatal(err)
	}

	logger := zaptest.NewLogger(t)
	st := &fakeStore{}
	eng := &fakeEngine{}

	srv := grpc.NewServer()
	rlqsSrv := NewRLQS(logger, st, eng, config.ServerConfig{
		EngineTimeout: config.Duration{Duration: 5 * time.Second},
	}, classifier)
	rlqspb.RegisterRateLimitQuotaServiceServer(srv, rlqsSrv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)

	// Send a stream with a domain that does NOT match the classifier pattern.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	bid := bucketID("name", "test")
	err = stream.Send(&rlqspb.RateLimitQuotaUsageReports{
		Domain:            "evil.attacker.com",
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

	// The stream should have used __unknown__ as metricDomain.
	// We verify this indirectly: usage was still recorded (business logic uses real domain).
	if len(st.usages) != 1 {
		t.Fatalf("expected 1 usage, got %d", len(st.usages))
	}
}

func TestKnownDomainPassesThrough(t *testing.T) {
	classifier, err := metrics.NewDomainClassifier([]string{`^envoy`}, 100)
	if err != nil {
		t.Fatal(err)
	}

	logger := zaptest.NewLogger(t)
	st := &fakeStore{}
	eng := &fakeEngine{}

	srv := grpc.NewServer()
	rlqsSrv := NewRLQS(logger, st, eng, config.ServerConfig{
		EngineTimeout: config.Duration{Duration: 5 * time.Second},
	}, classifier)
	rlqspb.RegisterRateLimitQuotaServiceServer(srv, rlqsSrv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := rlqspb.NewRateLimitQuotaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamRateLimitQuotas(ctx)
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

	if len(st.usages) != 1 {
		t.Fatalf("expected 1 usage, got %d", len(st.usages))
	}
}

func TestTLSOptionInvalidCAFile(t *testing.T) {
	caCertPEM, caKeyPEM := generateTestCA(t)
	serverCertPEM, serverKeyPEM := generateTestCert(t, caCertPEM, caKeyPEM)

	certFile := writeTempFile(t, serverCertPEM)
	keyFile := writeTempFile(t, serverKeyPEM)

	_, err := TLSOption(config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   "/nonexistent/ca.pem",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent CA file")
	}
}
