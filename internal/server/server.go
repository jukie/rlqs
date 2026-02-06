package server

import (
	"io"
	"log/slog"
	"net"

	"rlqs/internal/engine"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const serviceName = "envoy.service.rate_limit_quota.v3.RateLimitQuotaService"

// Server wraps a gRPC server implementing the RLQS protocol.
type Server struct {
	grpcServer   *grpc.Server
	healthServer *health.Server
	engine       *engine.Engine
	logger       *slog.Logger

	rlqspb.UnimplementedRateLimitQuotaServiceServer
}

func New(eng *engine.Engine, logger *slog.Logger) *Server {
	grpcSrv := grpc.NewServer()
	healthSrv := health.NewServer()

	s := &Server{
		grpcServer:   grpcSrv,
		healthServer: healthSrv,
		engine:       eng,
		logger:       logger,
	}

	rlqspb.RegisterRateLimitQuotaServiceServer(grpcSrv, s)
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)

	return s
}

// StreamRateLimitQuotas implements the bidirectional RLQS streaming RPC.
func (s *Server) StreamRateLimitQuotas(stream rlqspb.RateLimitQuotaService_StreamRateLimitQuotasServer) error {
	for {
		report, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		s.logger.Info("received usage report",
			"domain", report.GetDomain(),
			"buckets", len(report.GetBucketQuotaUsages()),
		)

		resp := s.engine.ProcessReport(report)
		if err := stream.Send(resp); err != nil {
			return err
		}
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
