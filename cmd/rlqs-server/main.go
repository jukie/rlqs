package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/jukie/rlqs/internal/server"
	"github.com/jukie/rlqs/internal/storage"
	"github.com/jukie/rlqs/internal/quota"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Sync()

	port := os.Getenv("RLQS_PORT")
	if port == "" {
		port = "18080"
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		logger.Fatal("failed to listen", zap.Error(err))
	}

	// TODO: replace with real implementations.
	var store storage.BucketStore
	var engine quota.Engine

	grpcServer := grpc.NewServer()
	server.Register(grpcServer, logger, store, engine)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", zap.String("signal", sig.String()))
		grpcServer.GracefulStop()
	}()

	logger.Info("starting RLQS server", zap.String("addr", lis.Addr().String()))
	if err := grpcServer.Serve(lis); err != nil {
		logger.Fatal("failed to serve", zap.Error(err))
	}
}
