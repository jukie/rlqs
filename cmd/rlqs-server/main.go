package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/policy"
	"github.com/jukie/rlqs/internal/quota"
	"github.com/jukie/rlqs/internal/server"
	"github.com/jukie/rlqs/internal/storage"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// buildStore creates a BucketStore from the configuration.
// Defaults to in-memory; supports "redis" for Redis-backed storage.
func buildStore(cfg *config.Config, logger *zap.Logger) storage.BucketStore {
	switch cfg.Storage.Type {
	case "redis":
		logger.Info("using redis storage backend",
			zap.String("addr", cfg.Storage.Redis.Addr),
			zap.Int("pool_size", cfg.Storage.Redis.PoolSize),
		)
		return storage.NewRedisStorage(storage.RedisConfig{
			Addr:     cfg.Storage.Redis.Addr,
			PoolSize: cfg.Storage.Redis.PoolSize,
			KeyTTL:   cfg.Engine.ReportingInterval.Duration * 6,
		})
	default:
		logger.Info("using in-memory storage backend")
		return storage.NewMemoryStorage()
	}
}

// buildEngine creates a quota engine from the configuration.
// If policies are defined, returns an Engine; otherwise returns DefaultEngine.
func buildEngine(cfg *config.Config) quota.Engine {
	defaultStrategy := &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_TokenBucket{
			TokenBucket: &typev3.TokenBucket{
				MaxTokens:     uint32(cfg.Engine.DefaultRPS),
				TokensPerFill: wrapperspb.UInt32(uint32(cfg.Engine.DefaultRPS)),
				FillInterval:  durationpb.New(cfg.Engine.ReportingInterval.Duration),
			},
		},
	}
	defaultTTL := cfg.Engine.ReportingInterval.Duration * 2

	// If no policies configured, use DefaultEngine
	if len(cfg.Engine.Policies) == 0 {
		return quota.NewDefaultEngine(quota.DefaultEngineConfig{
			DefaultStrategy: defaultStrategy,
			AssignmentTTL:   defaultTTL,
		})
	}

	// Build Engine from config
	policies := make([]policy.Policy, 0, len(cfg.Engine.Policies))
	for _, pc := range cfg.Engine.Policies {
		ttl := pc.AssignmentTTL.Duration
		if ttl == 0 {
			ttl = defaultTTL
		}

		policies = append(policies, policy.Policy{
			DomainPattern:    pc.DomainPattern,
			BucketKeyPattern: pc.BucketKeyPattern,
			Strategy: &typev3.RateLimitStrategy{
				Strategy: &typev3.RateLimitStrategy_TokenBucket{
					TokenBucket: &typev3.TokenBucket{
						MaxTokens:     uint32(pc.RPS),
						TokensPerFill: wrapperspb.UInt32(uint32(pc.RPS)),
						FillInterval:  durationpb.New(cfg.Engine.ReportingInterval.Duration),
					},
				},
			},
			AssignmentTTL: ttl,
		})
	}

	return policy.New(policy.EngineConfig{
		Policies: policies,
		DefaultPolicy: policy.Policy{
			Strategy:      defaultStrategy,
			AssignmentTTL: defaultTTL,
		},
	})
}

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

	store := buildStore(cfg, logger)
	eng := buildEngine(cfg)

	srv := server.New(logger, store, eng, server.DefaultServerOptions(logger)...)

	lis, err := net.Listen("tcp", cfg.Server.GRPCAddr)
	if err != nil {
		logger.Fatal("failed to listen", zap.String("addr", cfg.Server.GRPCAddr), zap.Error(err))
	}

	// Start metrics HTTP server.
	metricsServer := &http.Server{
		Addr:    cfg.Server.MetricsAddr,
		Handler: promhttp.Handler(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("starting metrics server", zap.String("addr", cfg.Server.MetricsAddr))
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", zap.Error(err))
		}
	}()

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

	// Shutdown metrics server.
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown error", zap.Error(err))
	}

	srv.GracefulStop()
	logger.Info("server stopped")
}
