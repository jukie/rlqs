package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"rlqs/internal/config"
	"rlqs/internal/engine"
	"rlqs/internal/server"
	"rlqs/internal/storage"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	store := storage.NewMemoryStorage()
	eng := engine.New(cfg.Engine, store)
	srv := server.New(eng, logger)

	lis, err := net.Listen("tcp", cfg.Server.GRPCAddr)
	if err != nil {
		logger.Error("failed to listen", "addr", cfg.Server.GRPCAddr, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("starting rlqs server", "addr", lis.Addr().String())
		if err := srv.Serve(lis); err != nil {
			logger.Error("server error", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	srv.GracefulStop()
	logger.Info("server stopped")
}
