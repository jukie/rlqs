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
