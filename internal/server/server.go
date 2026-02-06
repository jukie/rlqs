package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jukie/rlqs/internal/admin"
	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/metrics"
	"github.com/jukie/rlqs/internal/quota"
	"github.com/jukie/rlqs/internal/storage"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

const serviceName = "envoy.service.rate_limit_quota.v3.RateLimitQuotaService"

// === gRPC Interceptors ===

// recoveryStreamInterceptor converts panics in stream handlers to gRPC errors.
func recoveryStreamInterceptor(logger *zap.Logger) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in stream handler",
					zap.String("method", info.FullMethod),
					zap.Any("panic", r))
				err = status.Errorf(codes.Internal, "internal server error: %v", r)
			}
		}()
		return handler(srv, ss)
	}
}

// loggingStreamInterceptor logs stream RPC calls.
func loggingStreamInterceptor(logger *zap.Logger) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		logger.Debug("stream RPC started", zap.String("method", info.FullMethod))
		err := handler(srv, ss)
		if err != nil {
			logger.Debug("stream RPC completed with error",
				zap.String("method", info.FullMethod),
				zap.Error(err))
		} else {
			logger.Debug("stream RPC completed", zap.String("method", info.FullMethod))
		}
		return err
	}
}

// === RLQSServer: full RLQS protocol implementation (rictus) ===

// RLQSServer implements the Rate Limit Quota Service with full protocol support.
type RLQSServer struct {
	rlqspb.UnimplementedRateLimitQuotaServiceServer

	logger           *zap.Logger
	store            storage.BucketStore
	engine           atomic.Value // holds quota.Engine
	domainClassifier atomic.Value // holds *metrics.DomainClassifier

	maxBucketsPerStream  int
	maxReportsPerMessage int
	maxBucketEntries     int
	maxBucketKeyLen      int
	maxBucketValueLen    int
	engineTimeout        time.Duration

	// mu protects streams and bucketRefs.
	mu         sync.Mutex
	streams    map[*streamState]struct{}
	bucketRefs map[storage.BucketKey]int // domain-scoped key -> subscriber count
}

// streamState tracks the per-stream lifecycle.
type streamState struct {
	mu           sync.Mutex
	domain       string
	metricDomain string // bounded label for Prometheus (set once at domain binding)
	domainBound  bool

	// subscriptions tracks bucket keys the client has reported on this stream.
	subscriptions map[storage.BucketKey]struct{}
}

// NewRLQS creates a new RLQSServer with full protocol support.
// The classifier bounds Prometheus domain-label cardinality; if nil a
// default classifier with no patterns (all domains allowed up to cap) is used.
func NewRLQS(logger *zap.Logger, store storage.BucketStore, engine quota.Engine, cfg config.ServerConfig, classifier *metrics.DomainClassifier) *RLQSServer {
	if classifier == nil {
		// Fallback: allow all domains up to the configured cap.
		classifier, _ = metrics.NewDomainClassifier(nil, cfg.MaxMetricDomains)
	}
	s := &RLQSServer{
		logger:               logger,
		store:                store,
		maxBucketsPerStream:  cfg.MaxBucketsPerStream,
		maxReportsPerMessage: cfg.MaxReportsPerMessage,
		maxBucketEntries:     cfg.MaxBucketEntries,
		maxBucketKeyLen:      cfg.MaxBucketKeyLen,
		maxBucketValueLen:    cfg.MaxBucketValueLen,
		engineTimeout:        cfg.EngineTimeout.Duration,
		streams:              make(map[*streamState]struct{}),
		bucketRefs:           make(map[storage.BucketKey]int),
	}
	s.engine.Store(engine)
	s.domainClassifier.Store(classifier)
	return s
}

// SwapEngine atomically replaces the quota engine used for all subsequent
// ProcessUsage calls. Active streams pick up the new engine on their next
// message without interruption.
func (s *RLQSServer) SwapEngine(e quota.Engine) {
	s.engine.Store(e)
}

// SwapDomainClassifier atomically replaces the domain classifier used for
// Prometheus label normalization. Already-bound streams keep their existing
// metricDomain; new streams use the new classifier.
func (s *RLQSServer) SwapDomainClassifier(c *metrics.DomainClassifier) {
	s.domainClassifier.Store(c)
}

// classifyDomain returns a bounded Prometheus label for the given domain.
func (s *RLQSServer) classifyDomain(domain string) string {
	return s.domainClassifier.Load().(*metrics.DomainClassifier).Classify(domain)
}

// Register adds the RLQS service to the gRPC server using the full protocol implementation.
func Register(s *grpc.Server, logger *zap.Logger, store storage.BucketStore, engine quota.Engine, cfg config.ServerConfig) {
	rlqspb.RegisterRateLimitQuotaServiceServer(s, NewRLQS(logger, store, engine, cfg, nil))
}

// StreamRateLimitQuotas implements the bidirectional streaming RPC.
func (s *RLQSServer) StreamRateLimitQuotas(stream rlqspb.RateLimitQuotaService_StreamRateLimitQuotasServer) error {
	ss := &streamState{
		subscriptions: make(map[storage.BucketKey]struct{}),
	}

	s.mu.Lock()
	s.streams[ss] = struct{}{}
	s.mu.Unlock()

	metrics.ActiveStreams.Inc()
	defer func() {
		metrics.ActiveStreams.Dec()
		s.cleanupStream(ss)
	}()

	s.logger.Debug("stream opened")

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			s.logger.Debug("stream closed by client")
			return nil
		}
		if err != nil {
			s.logger.Debug("stream recv error", zap.Error(err))
			return err
		}

		if err := s.handleMessage(stream, ss, req); err != nil {
			return err
		}
	}
}

// handleMessage processes a single inbound usage report message.
func (s *RLQSServer) handleMessage(
	stream rlqspb.RateLimitQuotaService_StreamRateLimitQuotasServer,
	ss *streamState,
	req *rlqspb.RateLimitQuotaUsageReports,
) error {
	ctx := stream.Context()

	// Domain binding: the first message sets the domain for the stream.
	ss.mu.Lock()
	if !ss.domainBound {
		domain := req.GetDomain()
		if domain == "" {
			ss.mu.Unlock()
			return status.Error(codes.InvalidArgument, "first message must include a non-empty domain")
		}
		ss.domain = domain
		ss.metricDomain = s.classifyDomain(domain)
		ss.domainBound = true
		s.logger.Debug("stream bound to domain", zap.String("domain", domain))
	}

	// Validate domain consistency on subsequent messages.
	if d := req.GetDomain(); d != "" && d != ss.domain {
		ss.mu.Unlock()
		return status.Errorf(codes.InvalidArgument,
			"domain mismatch: stream bound to %q but received %q (open a new stream to change domain)",
			ss.domain, d)
	}
	ss.mu.Unlock()

	usages := req.GetBucketQuotaUsages()
	if len(usages) == 0 {
		return nil
	}

	// Enforce max reports per message to prevent unbounded batch sizes.
	if s.maxReportsPerMessage > 0 && len(usages) > s.maxReportsPerMessage {
		return status.Errorf(codes.ResourceExhausted,
			"too many usage reports in a single message: %d (limit: %d)", len(usages), s.maxReportsPerMessage)
	}

	// Build reports and track subscriptions.
	reports := make([]storage.UsageReport, 0, len(usages))
	for _, u := range usages {
		if u.GetBucketId() == nil || len(u.GetBucketId().GetBucket()) == 0 {
			continue
		}

		// Validate BucketId map bounds.
		bucket := u.GetBucketId().GetBucket()
		if s.maxBucketEntries > 0 && len(bucket) > s.maxBucketEntries {
			return status.Errorf(codes.InvalidArgument,
				"bucket_id has too many entries: %d (limit: %d)", len(bucket), s.maxBucketEntries)
		}
		for k, v := range bucket {
			if k == "" || v == "" {
				return status.Errorf(codes.InvalidArgument,
					"bucket_id keys and values must be non-empty")
			}
			if s.maxBucketKeyLen > 0 && len(k) > s.maxBucketKeyLen {
				return status.Errorf(codes.InvalidArgument,
					"bucket_id key exceeds max length: %d (limit: %d)", len(k), s.maxBucketKeyLen)
			}
			if s.maxBucketValueLen > 0 && len(v) > s.maxBucketValueLen {
				return status.Errorf(codes.InvalidArgument,
					"bucket_id value exceeds max length: %d (limit: %d)", len(v), s.maxBucketValueLen)
			}
		}
		key := storage.BucketKeyFromProto(u.GetBucketId())

		// Track subscription — first report for a bucket is the subscription signal.
		// Both ss.subscriptions and s.bucketRefs must be updated atomically
		// under s.mu to prevent a race where another stream's cleanupStream
		// sees a stale ref count and prematurely removes the bucket.
		scopedKey := storage.DomainScopedKey(ss.domain, key)
		s.mu.Lock()
		_, exists := ss.subscriptions[key]
		if !exists {
			if s.maxBucketsPerStream > 0 && len(ss.subscriptions) >= s.maxBucketsPerStream {
				s.mu.Unlock()
				return status.Errorf(codes.ResourceExhausted,
					"max buckets per stream exceeded (limit: %d)", s.maxBucketsPerStream)
			}
			ss.subscriptions[key] = struct{}{}
			s.bucketRefs[scopedKey]++
		}
		s.mu.Unlock()

		if !exists {
			metrics.ActiveBuckets.WithLabelValues(ss.metricDomain).Inc()
			s.logger.Debug("bucket subscribed",
				zap.String("domain", ss.domain),
				zap.String("bucket_key", string(key)))
		}

		report := storage.UsageReport{
			BucketId:           u.GetBucketId(),
			NumRequestsAllowed: u.GetNumRequestsAllowed(),
			NumRequestsDenied:  u.GetNumRequestsDenied(),
			TimeElapsed:        u.GetTimeElapsed().AsDuration(),
		}

		if err := s.store.RecordUsage(ctx, ss.domain, report); err != nil {
			s.logger.Error("failed to record usage",
				zap.String("domain", ss.domain),
				zap.String("bucket_key", string(key)),
				zap.Error(err))
		}

		reports = append(reports, report)
	}

	// Track usage reports processed.
	metrics.UsageReportsTotal.WithLabelValues(ss.metricDomain).Add(float64(len(reports)))

	// Consult the quota engine and track duration.
	engineCtx := ctx
	if s.engineTimeout > 0 {
		var cancel context.CancelFunc
		engineCtx, cancel = context.WithTimeout(ctx, s.engineTimeout)
		defer cancel()
	}
	start := time.Now()
	eng := s.engine.Load().(quota.Engine)
	actions, err := eng.ProcessUsage(engineCtx, ss.domain, reports)
	metrics.EngineDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		s.logger.Error("engine error", zap.String("domain", ss.domain), zap.Error(err))
		return status.Errorf(codes.Internal, "quota engine error: %v", err)
	}

	if len(actions) == 0 {
		return nil
	}

	// Track assignments and abandon actions.
	metrics.AssignmentsTotal.WithLabelValues(ss.metricDomain).Add(float64(len(actions)))
	for _, a := range actions {
		if a.GetAbandonAction() != nil {
			key := storage.BucketKeyFromProto(a.GetBucketId())
			ss.mu.Lock()
			if _, exists := ss.subscriptions[key]; exists {
				delete(ss.subscriptions, key)
				ss.mu.Unlock()
				scopedKey := storage.DomainScopedKey(ss.domain, key)
				s.mu.Lock()
				s.bucketRefs[scopedKey]--
				if s.bucketRefs[scopedKey] <= 0 {
					delete(s.bucketRefs, scopedKey)
					s.mu.Unlock()
					if err := s.store.RemoveBucket(ctx, ss.domain, key); err != nil {
						s.logger.Error("failed to remove abandoned bucket",
							zap.String("domain", ss.domain),
							zap.String("bucket_key", string(key)),
							zap.Error(err))
					}
				} else {
					s.mu.Unlock()
				}
				metrics.ActiveBuckets.WithLabelValues(ss.metricDomain).Dec()
				s.logger.Debug("bucket abandoned",
					zap.String("domain", ss.domain),
					zap.String("bucket_key", string(key)))
			} else {
				ss.mu.Unlock()
			}
		}
	}

	resp := &rlqspb.RateLimitQuotaResponse{
		BucketAction: actions,
	}
	if err := stream.Send(resp); err != nil {
		s.logger.Debug("stream send error", zap.Error(err))
		return err
	}

	return nil
}

// cleanupStream removes stream state and cleans up bucket subscriptions.
// Shared bucket storage is only removed when no other streams reference the bucket,
// preventing HA data loss when multiple RLQS instances share Redis state.
func (s *RLQSServer) cleanupStream(ss *streamState) {
	s.mu.Lock()
	delete(s.streams, ss)
	s.mu.Unlock()

	if !ss.domainBound {
		return
	}

	s.logger.Debug("cleaning up stream",
		zap.String("domain", ss.domain),
		zap.Int("subscriptions", len(ss.subscriptions)))

	for key := range ss.subscriptions {
		metrics.ActiveBuckets.WithLabelValues(ss.metricDomain).Dec()
		scopedKey := storage.DomainScopedKey(ss.domain, key)
		s.mu.Lock()
		s.bucketRefs[scopedKey]--
		shouldRemove := s.bucketRefs[scopedKey] <= 0
		if shouldRemove {
			delete(s.bucketRefs, scopedKey)
		}
		s.mu.Unlock()
		if shouldRemove {
			if err := s.store.RemoveBucket(context.Background(), ss.domain, key); err != nil {
				s.logger.Error("failed to remove bucket on disconnect",
					zap.String("domain", ss.domain),
					zap.String("bucket_key", string(key)),
					zap.Error(err))
			}
		}
	}
}

// StreamStats returns information about all active streams.
func (s *RLQSServer) StreamStats() []admin.StreamInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := make([]admin.StreamInfo, 0, len(s.streams))
	for ss := range s.streams {
		ss.mu.Lock()
		stats = append(stats, admin.StreamInfo{
			Domain:            ss.domain,
			SubscriptionCount: len(ss.subscriptions),
		})
		ss.mu.Unlock()
	}
	return stats
}

// DefaultServerOptions returns the standard set of server options with recovery, logging, Prometheus metrics,
// keepalive, and concurrency limits derived from the server config.
func DefaultServerOptions(logger *zap.Logger, cfg config.ServerConfig) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxConcurrentStreams(cfg.MaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: cfg.KeepaliveMaxIdleTime.Duration,
			Time:              cfg.KeepalivePingInterval.Duration,
			Timeout:           cfg.KeepalivePingTimeout.Duration,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainStreamInterceptor(
			grpc_prometheus.StreamServerInterceptor,
			loggingStreamInterceptor(logger),
			recoveryStreamInterceptor(logger),
		),
		grpc.ChainUnaryInterceptor(
			grpc_prometheus.UnaryServerInterceptor,
		),
	}
}

// === Server: high-level wrapper with health checks ===

// Server wraps an RLQSServer with health checks and lifecycle management.
type Server struct {
	grpcServer   *grpc.Server
	healthServer *health.Server
	rlqsServer   *RLQSServer
}

// New creates a Server that uses the full-protocol RLQSServer with health checks.
// classifier may be nil; see NewRLQS.
func New(logger *zap.Logger, store storage.BucketStore, engine quota.Engine, cfg config.ServerConfig, classifier *metrics.DomainClassifier, opts ...grpc.ServerOption) *Server {
	grpcSrv := grpc.NewServer(opts...)
	healthSrv := health.NewServer()
	rlqsSrv := NewRLQS(logger, store, engine, cfg, classifier)

	rlqspb.RegisterRateLimitQuotaServiceServer(grpcSrv, rlqsSrv)
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)

	// Initialize gRPC Prometheus metrics.
	grpc_prometheus.Register(grpcSrv)

	return &Server{
		grpcServer:   grpcSrv,
		healthServer: healthSrv,
		rlqsServer:   rlqsSrv,
	}
}

// SwapEngine atomically replaces the quota engine. Active streams pick up
// the new engine on their next message.
func (s *Server) SwapEngine(e quota.Engine) {
	s.rlqsServer.SwapEngine(e)
}

// SwapDomainClassifier atomically replaces the domain classifier for
// Prometheus label normalization.
func (s *Server) SwapDomainClassifier(c *metrics.DomainClassifier) {
	s.rlqsServer.SwapDomainClassifier(c)
}

// StreamStats returns information about all active streams.
func (s *Server) StreamStats() []admin.StreamInfo {
	return s.rlqsServer.StreamStats()
}

// Serve starts the gRPC server on the given listener.
func (s *Server) Serve(lis net.Listener) error {
	s.healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	s.healthServer.SetServingStatus(serviceName, healthpb.HealthCheckResponse_SERVING)
	return s.grpcServer.Serve(lis)
}

// GracefulStop drains connections and stops the server.
func (s *Server) GracefulStop() {
	s.healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	s.healthServer.SetServingStatus(serviceName, healthpb.HealthCheckResponse_NOT_SERVING)
	s.grpcServer.GracefulStop()
}

// Stop forcefully stops the gRPC server without waiting for active streams.
func (s *Server) Stop() {
	s.grpcServer.Stop()
}

// GRPCServer returns the underlying grpc.Server (for testing).
func (s *Server) GRPCServer() *grpc.Server {
	return s.grpcServer
}

// TLSOption builds a grpc.ServerOption from TLS configuration.
// If CAFile is set, mTLS is enforced (clients must present a valid certificate).
// Returns nil if no TLS is configured (CertFile is empty).
func TLSOption(tlsCfg config.TLSConfig) (grpc.ServerOption, error) {
	cert, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading server certificate: %w", err)
	}

	tc := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if tlsCfg.CAFile != "" {
		caPEM, err := os.ReadFile(tlsCfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("CA file contains no valid certificates")
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return grpc.Creds(credentials.NewTLS(tc)), nil
}
