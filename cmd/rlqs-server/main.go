package main

import (
	"context"
	"flag"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/quota"
	"github.com/jukie/rlqs/internal/server"
	"github.com/jukie/rlqs/internal/storage"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}

	store := storage.NewMemoryStorage()
	eng := quota.NewDefaultEngine(quota.DefaultEngineConfig{
		DefaultStrategy: &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_TokenBucket{
				TokenBucket: &typev3.TokenBucket{
					MaxTokens:     uint32(cfg.Engine.DefaultRPS),
					TokensPerFill: wrapperspb.UInt32(uint32(cfg.Engine.DefaultRPS)),
					FillInterval:  durationpb.New(cfg.Engine.ReportingInterval.Duration),
				},
			},
		},
		AssignmentTTL: cfg.Engine.ReportingInterval.Duration * 2,
	})

	srv := server.New(logger, store, eng, server.DefaultServerOptions(logger)...)

	lis, err := net.Listen("tcp", cfg.Server.GRPCAddr)
	if err != nil {
		logger.Fatal("failed to listen", zap.String("addr", cfg.Server.GRPCAddr), zap.Error(err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("starting rlqs server", zap.String("addr", lis.Addr().String()))
		if err := srv.Serve(lis); err != nil {
			logger.Error("server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	// Give active streams time to drain.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = shutdownCtx

	srv.GracefulStop()
	logger.Info("server stopped")
}
