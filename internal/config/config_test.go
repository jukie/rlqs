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

func TestLoadTracingYAML(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	_, err = f.WriteString(`tracing:
  enabled: true
  endpoint: "otel-collector:4317"
  insecure: true
`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tracing.Enabled {
		t.Fatal("expected tracing enabled")
	}
	if cfg.Tracing.Endpoint != "otel-collector:4317" {
		t.Fatalf("expected otel-collector:4317, got %s", cfg.Tracing.Endpoint)
	}
	if !cfg.Tracing.Insecure {
		t.Fatal("expected tracing insecure")
	}
}

func TestLoadTracingEnvOverrides(t *testing.T) {
	t.Setenv("RLQS_TRACING_ENABLED", "true")
	t.Setenv("RLQS_TRACING_ENDPOINT", "localhost:4317")
	t.Setenv("RLQS_TRACING_INSECURE", "1")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tracing.Enabled {
		t.Fatal("expected tracing enabled via env")
	}
	if cfg.Tracing.Endpoint != "localhost:4317" {
		t.Fatalf("expected localhost:4317, got %s", cfg.Tracing.Endpoint)
	}
	if !cfg.Tracing.Insecure {
		t.Fatal("expected tracing insecure via env")
	}
}

func TestLoadPolicyWithDenyResponse(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	_, err = f.WriteString(`engine:
  default_rps: 100
  reporting_interval: "10s"
  policies:
    - domain_pattern: "^blocked\\."
      strategy: "deny"
      deny_response:
        http_status: 403
        http_body: '{"error": "forbidden"}'
        grpc_status_code: 7
        grpc_status_message: "permission denied"
        response_headers_to_add:
          X-Rate-Limit-Reason: "blocked"
          Retry-After: "3600"
    - domain_pattern: "^open\\."
      strategy: "allow"
    - domain_pattern: "^api\\."
      rps: 500
`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Engine.Policies) != 3 {
		t.Fatalf("expected 3 policies, got %d", len(cfg.Engine.Policies))
	}

	// Deny policy
	deny := cfg.Engine.Policies[0]
	if deny.Strategy != "deny" {
		t.Fatalf("expected strategy 'deny', got %q", deny.Strategy)
	}
	if deny.DenyResponse == nil {
		t.Fatal("expected deny_response to be set")
	}
	if deny.DenyResponse.HTTPStatus != 403 {
		t.Fatalf("expected http_status 403, got %d", deny.DenyResponse.HTTPStatus)
	}
	if deny.DenyResponse.HTTPBody != `{"error": "forbidden"}` {
		t.Fatalf("unexpected http_body: %s", deny.DenyResponse.HTTPBody)
	}
	if deny.DenyResponse.GRPCStatusCode != 7 {
		t.Fatalf("expected grpc_status_code 7, got %d", deny.DenyResponse.GRPCStatusCode)
	}
	if deny.DenyResponse.GRPCStatusMessage != "permission denied" {
		t.Fatalf("expected grpc_status_message 'permission denied', got %q", deny.DenyResponse.GRPCStatusMessage)
	}
	if len(deny.DenyResponse.ResponseHeadersToAdd) != 2 {
		t.Fatalf("expected 2 response headers, got %d", len(deny.DenyResponse.ResponseHeadersToAdd))
	}
	if deny.DenyResponse.ResponseHeadersToAdd["X-Rate-Limit-Reason"] != "blocked" {
		t.Fatalf("unexpected header value: %s", deny.DenyResponse.ResponseHeadersToAdd["X-Rate-Limit-Reason"])
	}

	// Allow policy
	allow := cfg.Engine.Policies[1]
	if allow.Strategy != "allow" {
		t.Fatalf("expected strategy 'allow', got %q", allow.Strategy)
	}
	if allow.DenyResponse != nil {
		t.Fatal("expected no deny_response for allow policy")
	}

	// Token bucket policy (default strategy)
	tb := cfg.Engine.Policies[2]
	if tb.Strategy != "" {
		t.Fatalf("expected empty strategy (default), got %q", tb.Strategy)
	}
	if tb.RPS != 500 {
		t.Fatalf("expected rps 500, got %d", tb.RPS)
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
