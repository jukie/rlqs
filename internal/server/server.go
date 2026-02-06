package server

import (
	"context"
	"io"
	"sync"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jukie/rlqs/internal/quota"
	"github.com/jukie/rlqs/internal/storage"
)

// RLQSServer implements the Rate Limit Quota Service.
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

// New creates a new RLQSServer.
func New(logger *zap.Logger, store storage.BucketStore, engine quota.Engine) *RLQSServer {
	return &RLQSServer{
		logger:  logger,
		store:   store,
		engine:  engine,
		streams: make(map[*streamState]struct{}),
	}
}

// Register adds the RLQS service to the gRPC server.
func Register(s *grpc.Server, logger *zap.Logger, store storage.BucketStore, engine quota.Engine) {
	rlqspb.RegisterRateLimitQuotaServiceServer(s, New(logger, store, engine))
}

// StreamRateLimitQuotas implements the bidirectional streaming RPC.
func (s *RLQSServer) StreamRateLimitQuotas(stream rlqspb.RateLimitQuotaService_StreamRateLimitQuotasServer) error {
	ss := &streamState{
		subscriptions: make(map[storage.BucketKey]struct{}),
	}

	s.mu.Lock()
	s.streams[ss] = struct{}{}
	s.mu.Unlock()

	defer s.cleanupStream(ss)

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
		if u.GetBucketId() == nil {
			continue
		}
		key := storage.BucketKeyFromProto(u.GetBucketId())

		// Track subscription — first report for a bucket is the subscription signal.
		if _, exists := ss.subscriptions[key]; !exists {
			ss.subscriptions[key] = struct{}{}
			s.logger.Debug("bucket subscribed",
				zap.String("domain", ss.domain),
				zap.String("bucket_key", string(key)))
		}

		report := storage.UsageReport{
			BucketId:           u.GetBucketId(),
			NumRequestsAllowed: u.GetNumRequestsAllowed(),
			NumRequestsDenied:  u.GetNumRequestsDenied(),
		}

		if err := s.store.RecordUsage(ctx, ss.domain, report); err != nil {
			s.logger.Error("failed to record usage",
				zap.String("domain", ss.domain),
				zap.String("bucket_key", string(key)),
				zap.Error(err))
		}

		reports = append(reports, report)
	}

	// Consult the quota engine.
	actions, err := s.engine.ProcessUsage(ctx, ss.domain, reports)
	if err != nil {
		s.logger.Error("engine error", zap.String("domain", ss.domain), zap.Error(err))
		return status.Errorf(codes.Internal, "quota engine error: %v", err)
	}

	if len(actions) == 0 {
		return nil
	}

	// Track abandon actions — remove from subscriptions.
	for _, a := range actions {
		if a.GetAbandonAction() != nil {
			key := storage.BucketKeyFromProto(a.GetBucketId())
			delete(ss.subscriptions, key)
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
		if err := s.store.RemoveBucket(context.Background(), ss.domain, key); err != nil {
			s.logger.Error("failed to remove bucket on disconnect",
				zap.String("domain", ss.domain),
				zap.String("bucket_key", string(key)),
				zap.Error(err))
		}
	}
}
