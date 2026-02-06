package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.GRPCAddr != ":18081" {
		t.Fatalf("expected :18081, got %s", cfg.Server.GRPCAddr)
	}
	if cfg.Engine.DefaultRPS != 100 {
		t.Fatalf("expected 100, got %d", cfg.Engine.DefaultRPS)
	}
	if cfg.Engine.ReportingInterval.Duration != 10*time.Second {
		t.Fatalf("expected 10s, got %v", cfg.Engine.ReportingInterval)
	}
}

func TestLoadYAML(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	_, err = f.WriteString(`server:
  grpc_addr: ":9090"
engine:
  default_rps: 50
  reporting_interval: "5s"
`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.GRPCAddr != ":9090" {
		t.Fatalf("expected :9090, got %s", cfg.Server.GRPCAddr)
	}
	if cfg.Engine.DefaultRPS != 50 {
		t.Fatalf("expected 50, got %d", cfg.Engine.DefaultRPS)
	}
	if cfg.Engine.ReportingInterval.Duration != 5*time.Second {
		t.Fatalf("expected 5s, got %v", cfg.Engine.ReportingInterval)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("RLQS_GRPC_ADDR", ":7070")
	t.Setenv("RLQS_DEFAULT_RPS", "200")
	t.Setenv("RLQS_REPORTING_INTERVAL", "30s")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.GRPCAddr != ":7070" {
		t.Fatalf("expected :7070, got %s", cfg.Server.GRPCAddr)
	}
	if cfg.Engine.DefaultRPS != 200 {
		t.Fatalf("expected 200, got %d", cfg.Engine.DefaultRPS)
	}
	if cfg.Engine.ReportingInterval.Duration != 30*time.Second {
		t.Fatalf("expected 30s, got %v", cfg.Engine.ReportingInterval)
	}
}

func TestLoadStorageYAML(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	_, err = f.WriteString(`storage:
  type: redis
  redis:
    addr: "localhost:6379"
    pool_size: 20
`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Type != "redis" {
		t.Fatalf("expected redis, got %s", cfg.Storage.Type)
	}
	if cfg.Storage.Redis.Addr != "localhost:6379" {
		t.Fatalf("expected localhost:6379, got %s", cfg.Storage.Redis.Addr)
	}
	if cfg.Storage.Redis.PoolSize != 20 {
		t.Fatalf("expected 20, got %d", cfg.Storage.Redis.PoolSize)
	}
}

func TestLoadStorageEnvOverrides(t *testing.T) {
	t.Setenv("RLQS_STORAGE_TYPE", "redis")
	t.Setenv("RLQS_STORAGE_REDIS_ADDR", "redis.example.com:6379")
	t.Setenv("RLQS_STORAGE_REDIS_POOL_SIZE", "50")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Type != "redis" {
		t.Fatalf("expected redis, got %s", cfg.Storage.Type)
	}
	if cfg.Storage.Redis.Addr != "redis.example.com:6379" {
		t.Fatalf("expected redis.example.com:6379, got %s", cfg.Storage.Redis.Addr)
	}
	if cfg.Storage.Redis.PoolSize != 50 {
		t.Fatalf("expected 50, got %d", cfg.Storage.Redis.PoolSize)
	}
}

func TestLoadTLSYAML(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	_, err = f.WriteString(`server:
  tls:
    cert_file: "/path/to/cert.pem"
    key_file: "/path/to/key.pem"
    ca_file: "/path/to/ca.pem"
`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TLS.CertFile != "/path/to/cert.pem" {
		t.Fatalf("expected /path/to/cert.pem, got %s", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.KeyFile != "/path/to/key.pem" {
		t.Fatalf("expected /path/to/key.pem, got %s", cfg.Server.TLS.KeyFile)
	}
	if cfg.Server.TLS.CAFile != "/path/to/ca.pem" {
		t.Fatalf("expected /path/to/ca.pem, got %s", cfg.Server.TLS.CAFile)
	}
}

func TestLoadTLSEnvOverrides(t *testing.T) {
	t.Setenv("RLQS_TLS_CERT_FILE", "/env/cert.pem")
	t.Setenv("RLQS_TLS_KEY_FILE", "/env/key.pem")
	t.Setenv("RLQS_TLS_CA_FILE", "/env/ca.pem")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TLS.CertFile != "/env/cert.pem" {
		t.Fatalf("expected /env/cert.pem, got %s", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.KeyFile != "/env/key.pem" {
		t.Fatalf("expected /env/key.pem, got %s", cfg.Server.TLS.KeyFile)
	}
	if cfg.Server.TLS.CAFile != "/env/ca.pem" {
		t.Fatalf("expected /env/ca.pem, got %s", cfg.Server.TLS.CAFile)
	}
}

func TestLoadEnvOverridesYAML(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	_, err = f.WriteString(`server:
  grpc_addr: ":9090"
engine:
  default_rps: 50
  reporting_interval: "5s"
`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	t.Setenv("RLQS_GRPC_ADDR", ":7070")

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.GRPCAddr != ":7070" {
		t.Fatalf("env should override YAML: expected :7070, got %s", cfg.Server.GRPCAddr)
	}
	if cfg.Engine.DefaultRPS != 50 {
		t.Fatalf("expected YAML value 50, got %d", cfg.Engine.DefaultRPS)
	}
}
