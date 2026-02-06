package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jukie/rlqs/internal/admin"
	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/metrics"
	"github.com/jukie/rlqs/internal/policy"
	"github.com/jukie/rlqs/internal/quota"
	"github.com/jukie/rlqs/internal/server"
	"github.com/jukie/rlqs/internal/storage"
	"github.com/jukie/rlqs/internal/tracing"

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
			Logger:   logger,
		})
	default:
		logger.Info("using in-memory storage backend")
		return storage.NewMemoryStorage()
	}
}

// buildEngine creates a quota engine from the configuration.
// If policies are defined, returns an Engine; otherwise returns DefaultEngine.
func buildEngine(cfg *config.Config) (quota.Engine, error) {
	defaultStrategy := &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_TokenBucket{
			TokenBucket: &typev3.TokenBucket{
				MaxTokens:     uint32(cfg.Engine.DefaultTokensPerFill),
				TokensPerFill: wrapperspb.UInt32(uint32(cfg.Engine.DefaultTokensPerFill)),
				FillInterval:  durationpb.New(cfg.Engine.ReportingInterval.Duration),
			},
		},
	}
	defaultTTL := cfg.Engine.ReportingInterval.Duration * 2

	// If no policies configured, use DefaultEngine
	if len(cfg.Engine.Policies) == 0 {
		return quota.NewDefaultEngine(quota.DefaultEngineConfig{
			DefaultStrategy: defaultStrategy,
			AssignmentTTL:   &defaultTTL,
		}), nil
	}

	// Build Engine from config
	policies := make([]policy.Policy, 0, len(cfg.Engine.Policies))
	for _, pc := range cfg.Engine.Policies {
		// Resolve TTL: explicit config value wins, otherwise use default.
		var ttl *time.Duration
		if pc.AssignmentTTL != nil {
			d := pc.AssignmentTTL.Duration
			ttl = &d
		} else {
			ttl = &defaultTTL
		}

		strategy := buildStrategy(pc, cfg.Engine.ReportingInterval.Duration)

		policies = append(policies, policy.Policy{
			DomainPattern:    pc.DomainPattern,
			BucketKeyPattern: pc.BucketKeyPattern,
			Strategy:         strategy,
			AssignmentTTL:    ttl,
			DenyResponse:     pc.DenyResponse,
		})
	}

	return policy.New(policy.EngineConfig{
		Policies: policies,
		DefaultPolicy: policy.Policy{
			Strategy:      defaultStrategy,
			AssignmentTTL: &defaultTTL,
		},
	})
}

// buildStrategy creates a RateLimitStrategy from a PolicyConfig.
// Supports "deny" (DENY_ALL), "allow" (ALLOW_ALL), "requests_per_time_unit",
// and "token_bucket" (default).
func buildStrategy(pc config.PolicyConfig, reportingInterval time.Duration) *typev3.RateLimitStrategy {
	switch pc.Strategy {
	case "deny":
		return &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_BlanketRule_{
				BlanketRule: typev3.RateLimitStrategy_DENY_ALL,
			},
		}
	case "allow":
		return &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_BlanketRule_{
				BlanketRule: typev3.RateLimitStrategy_ALLOW_ALL,
			},
		}
	case "requests_per_time_unit":
		return &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_RequestsPerTimeUnit_{
				RequestsPerTimeUnit: &typev3.RateLimitStrategy_RequestsPerTimeUnit{
					RequestsPerTimeUnit: pc.RequestsPerTimeUnit,
					TimeUnit:            parseTimeUnit(pc.TimeUnit),
				},
			},
		}
	default: // "token_bucket" or empty
		return &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_TokenBucket{
				TokenBucket: &typev3.TokenBucket{
					MaxTokens:     uint32(pc.TokensPerFill),
					TokensPerFill: wrapperspb.UInt32(uint32(pc.TokensPerFill)),
					FillInterval:  durationpb.New(reportingInterval),
				},
			},
		}
	}
}

// parseTimeUnit converts a YAML time unit string to the proto enum value.
func parseTimeUnit(s string) typev3.RateLimitUnit {
	switch strings.ToUpper(s) {
	case "SECOND":
		return typev3.RateLimitUnit_SECOND
	case "MINUTE":
		return typev3.RateLimitUnit_MINUTE
	case "HOUR":
		return typev3.RateLimitUnit_HOUR
	case "DAY":
		return typev3.RateLimitUnit_DAY
	case "MONTH":
		return typev3.RateLimitUnit_MONTH
	case "YEAR":
		return typev3.RateLimitUnit_YEAR
	default:
		return typev3.RateLimitUnit_SECOND
	}
}

// buildDomainClassifier extracts domain patterns from policies and creates a
// DomainClassifier that bounds Prometheus label cardinality.
func buildDomainClassifier(cfg *config.Config) (*metrics.DomainClassifier, error) {
	patterns := make([]string, 0, len(cfg.Engine.Policies))
	for _, pc := range cfg.Engine.Policies {
		if pc.DomainPattern != "" {
			patterns = append(patterns, pc.DomainPattern)
		}
	}
	return metrics.NewDomainClassifier(patterns, cfg.Server.MaxMetricDomains)
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

	shutdownTracing, err := tracing.Init(context.Background(), cfg.Tracing)
	if err != nil {
		logger.Fatal("failed to initialize tracing", zap.Error(err))
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			logger.Error("tracing shutdown error", zap.Error(err))
		}
	}()
	if cfg.Tracing.Enabled {
		logger.Info("tracing enabled", zap.String("endpoint", cfg.Tracing.Endpoint))
	}

	store := buildStore(cfg, logger)
	eng, err := buildEngine(cfg)
	if err != nil {
		logger.Fatal("failed to build quota engine", zap.Error(err))
	}

	classifier, err := buildDomainClassifier(cfg)
	if err != nil {
		logger.Fatal("failed to build domain classifier", zap.Error(err))
	}

	opts := server.DefaultServerOptions(logger, cfg.Server)
	if cfg.Server.TLS.CertFile != "" {
		tlsOpt, err := server.TLSOption(cfg.Server.TLS)
		if err != nil {
			logger.Fatal("failed to configure TLS", zap.Error(err))
		}
		opts = append(opts, tlsOpt)
		logger.Info("TLS enabled",
			zap.String("cert", cfg.Server.TLS.CertFile),
			zap.Bool("mtls", cfg.Server.TLS.CAFile != ""))
	}

	srv := server.New(logger, store, eng, cfg.Server, classifier, opts...)

	lis, err := net.Listen("tcp", cfg.Server.GRPCAddr)
	if err != nil {
		logger.Fatal("failed to listen", zap.String("addr", cfg.Server.GRPCAddr), zap.Error(err))
	}

	// Start HTTP server with metrics and admin endpoints.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	adminHandler := admin.New(srv.StreamStats, store, cfg)
	adminHandler.Register(mux)

	metricsServer := &http.Server{
		Addr:    cfg.Server.MetricsAddr,
		Handler: mux,
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

	// Start config hot-reload watcher if a config file was specified.
	if *configPath != "" {
		watcher := config.NewWatcher(*configPath, logger)
		cfgCh, err := watcher.Watch(ctx)
		if err != nil {
			logger.Error("failed to start config watcher, hot-reload disabled", zap.Error(err))
		} else {
			go func() {
				currentCfg := cfg
				for newCfg := range cfgCh {
					changes := config.Diff(currentCfg, newCfg)
					if len(changes) == 0 {
						logger.Info("hot-reload: config file changed but no effective differences detected")
						continue
					}

					for _, c := range changes {
						if c.Scope == config.ScopeRequiresRestart {
							logger.Warn("hot-reload: config section changed but requires server restart to take effect",
								zap.String("section", c.Section))
						} else {
							logger.Info("hot-reload: applying config change",
								zap.String("section", c.Section))
						}
					}

					// Collect restart-required changes for the admin endpoint.
					var restartChanges []config.Change
					for _, c := range changes {
						if c.Scope == config.ScopeRequiresRestart {
							restartChanges = append(restartChanges, c)
						}
					}
					adminHandler.SetPendingRestartChanges(restartChanges)

					newEngine, err := buildEngine(newCfg)
					if err != nil {
						logger.Error("hot-reload: invalid policy config, keeping current engine", zap.Error(err))
						continue
					}
					newClassifier, err := buildDomainClassifier(newCfg)
					if err != nil {
						logger.Error("hot-reload: invalid domain classifier config, keeping current", zap.Error(err))
						continue
					}
					srv.SwapEngine(newEngine)
					srv.SwapDomainClassifier(newClassifier)
					currentCfg = newCfg
					logger.Info("engine hot-reloaded with new config")
				}
			}()
		}
	}

	<-ctx.Done()
	logger.Info("shutting down")

	// Give active streams time to drain.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Shutdown metrics server.
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown error", zap.Error(err))
	}

	// GracefulStop blocks until all streams close. Run it in a goroutine
	// with a deadline so we fall back to forceful Stop if it hangs.
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		logger.Info("server stopped gracefully")
	case <-shutdownCtx.Done():
		logger.Warn("graceful shutdown timed out, forcing stop")
		srv.Stop()
	}
}
