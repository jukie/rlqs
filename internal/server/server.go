package server

import (
	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// RLQSServer implements the Rate Limit Quota Service.
type RLQSServer struct {
	rlqspb.UnimplementedRateLimitQuotaServiceServer
	logger *zap.Logger
}

// New creates a new RLQSServer.
func New(logger *zap.Logger) *RLQSServer {
	return &RLQSServer{
		logger: logger,
	}
}

// Register adds the RLQS service to the gRPC server.
func Register(s *grpc.Server, logger *zap.Logger) {
	rlqspb.RegisterRateLimitQuotaServiceServer(s, New(logger))
}
