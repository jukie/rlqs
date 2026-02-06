package server

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/jukie/rlqs/internal/metrics"
	"github.com/jukie/rlqs/internal/quota"
	"github.com/jukie/rlqs/internal/storage"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
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

	logger *zap.Logger
	store  storage.BucketStore
	engine quota.Engine

	// mu protects streams.
	mu      sync.Mutex
	streams map[*streamState]struct{}
}

// streamState tracks the per-stream lifecycle.
type streamState struct {
	domain      string
	domainBound bool

	// subscriptions tracks bucket keys the client has reported on this stream.
	subscriptions map[storage.BucketKey]struct{}
}

// NewRLQS creates a new RLQSServer with full protocol support.
func NewRLQS(logger *zap.Logger, store storage.BucketStore, engine quota.Engine) *RLQSServer {
	return &RLQSServer{
		logger:  logger,
		store:   store,
		engine:  engine,
		streams: make(map[*streamState]struct{}),
	}
}

// Register adds the RLQS service to the gRPC server using the full protocol implementation.
func Register(s *grpc.Server, logger *zap.Logger, store storage.BucketStore, engine quota.Engine) {
	rlqspb.RegisterRateLimitQuotaServiceServer(s, NewRLQS(logger, store, engine))
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
	if !ss.domainBound {
		domain := req.GetDomain()
		if domain == "" {
			return status.Error(codes.InvalidArgument, "first message must include a non-empty domain")
		}
		ss.domain = domain
		ss.domainBound = true
		s.logger.Debug("stream bound to domain", zap.String("domain", domain))
	}

	// Validate domain consistency on subsequent messages.
	if d := req.GetDomain(); d != "" && d != ss.domain {
		return status.Errorf(codes.InvalidArgument,
			"domain mismatch: stream bound to %q but received %q (open a new stream to change domain)",
			ss.domain, d)
	}

	usages := req.GetBucketQuotaUsages()
	if len(usages) == 0 {
		return nil
	}

	// Build reports and track subscriptions.
	reports := make([]storage.UsageReport, 0, len(usages))
	for _, u := range usages {
		if u.GetBucketId() == nil || len(u.GetBucketId().GetBucket()) == 0 {
			continue
		}
		key := storage.BucketKeyFromProto(u.GetBucketId())

		// Track subscription — first report for a bucket is the subscription signal.
		if _, exists := ss.subscriptions[key]; !exists {
			ss.subscriptions[key] = struct{}{}
			metrics.ActiveBuckets.WithLabelValues(ss.domain).Inc()
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
	metrics.UsageReportsTotal.WithLabelValues(ss.domain).Add(float64(len(reports)))

	// Consult the quota engine and track duration.
	start := time.Now()
	actions, err := s.engine.ProcessUsage(ctx, ss.domain, reports)
	metrics.EngineDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		s.logger.Error("engine error", zap.String("domain", ss.domain), zap.Error(err))
		return status.Errorf(codes.Internal, "quota engine error: %v", err)
	}

	if len(actions) == 0 {
		return nil
	}

	// Track assignments and abandon actions.
	metrics.AssignmentsTotal.WithLabelValues(ss.domain).Add(float64(len(actions)))
	for _, a := range actions {
		if a.GetAbandonAction() != nil {
			key := storage.BucketKeyFromProto(a.GetBucketId())
			delete(ss.subscriptions, key)
			metrics.ActiveBuckets.WithLabelValues(ss.domain).Dec()
			s.logger.Debug("bucket abandoned",
				zap.String("domain", ss.domain),
				zap.String("bucket_key", string(key)))
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
		metrics.ActiveBuckets.WithLabelValues(ss.domain).Dec()
		if err := s.store.RemoveBucket(context.Background(), ss.domain, key); err != nil {
			s.logger.Error("failed to remove bucket on disconnect",
				zap.String("domain", ss.domain),
				zap.String("bucket_key", string(key)),
				zap.Error(err))
		}
	}
}

// DefaultServerOptions returns the standard set of server options with recovery, logging, and Prometheus metrics.
func DefaultServerOptions(logger *zap.Logger) []grpc.ServerOption {
	return []grpc.ServerOption{
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
}

// New creates a Server that uses the full-protocol RLQSServer with health checks.
func New(logger *zap.Logger, store storage.BucketStore, engine quota.Engine, opts ...grpc.ServerOption) *Server {
	grpcSrv := grpc.NewServer(opts...)
	healthSrv := health.NewServer()

	Register(grpcSrv, logger, store, engine)
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)

	// Initialize gRPC Prometheus metrics.
	grpc_prometheus.Register(grpcSrv)

	return &Server{
		grpcServer:   grpcSrv,
		healthServer: healthSrv,
	}
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

// GRPCServer returns the underlying grpc.Server (for testing).
func (s *Server) GRPCServer() *grpc.Server {
	return s.grpcServer
}
