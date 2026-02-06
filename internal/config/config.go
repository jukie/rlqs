package config

import (
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Engine  EngineConfig  `yaml:"engine"`
	Storage StorageConfig `yaml:"storage"`
	Tracing TracingConfig `yaml:"tracing"`
}

type TracingConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"` // OTLP gRPC endpoint (e.g. "localhost:4317")
	Insecure bool   `yaml:"insecure"` // Use insecure connection to collector
}

type StorageConfig struct {
	Type  string      `yaml:"type"`  // "memory" (default) or "redis"
	Redis RedisConfig `yaml:"redis"` // Redis-specific settings
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	PoolSize int    `yaml:"pool_size"`
}

type ServerConfig struct {
	GRPCAddr    string    `yaml:"grpc_addr"`
	MetricsAddr string    `yaml:"metrics_addr"`
	TLS         TLSConfig `yaml:"tls"`

	// Backpressure / server protection
	MaxConcurrentStreams uint32   `yaml:"max_concurrent_streams"`
	MaxBucketsPerStream  int      `yaml:"max_buckets_per_stream"`
	MaxReportsPerMessage int      `yaml:"max_reports_per_message"`
	EngineTimeout        Duration `yaml:"engine_timeout"`

	// BucketId validation limits
	MaxBucketEntries  int `yaml:"max_bucket_entries"`
	MaxBucketKeyLen   int `yaml:"max_bucket_key_len"`
	MaxBucketValueLen int `yaml:"max_bucket_value_len"`

	// Prometheus cardinality protection
	MaxMetricDomains int `yaml:"max_metric_domains"`

	// Keepalive
	KeepaliveMaxIdleTime  Duration `yaml:"keepalive_max_idle_time"`
	KeepalivePingInterval Duration `yaml:"keepalive_ping_interval"`
	KeepalivePingTimeout  Duration `yaml:"keepalive_ping_timeout"`
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

type EngineConfig struct {
	// DefaultTokensPerFill is the number of tokens added per reporting interval for the
	// default token bucket strategy. This maps directly to the TokenBucket.tokens_per_fill
	// proto field, with FillInterval set to ReportingInterval.
	// For example, DefaultTokensPerFill=100 with ReportingInterval=10s means 100 tokens
	// per 10 seconds (10 tokens/second), NOT 100 tokens/second.
	DefaultTokensPerFill uint64 `yaml:"default_tokens_per_fill"`

	// DefaultRPS is deprecated: use DefaultTokensPerFill instead. The name was misleading
	// because the value represents tokens per reporting interval, not requests per second.
	DefaultRPS uint64 `yaml:"default_rps"`

	ReportingInterval Duration       `yaml:"reporting_interval"`
	Policies          []PolicyConfig `yaml:"policies"`
}

// DenyResponseConfig customizes the response returned when a request is denied.
// These settings correspond to the Envoy RateLimitQuotaBucketSettings.DenyResponseSettings proto.
type DenyResponseConfig struct {
	// HTTPStatus is the HTTP status code to return for denied requests.
	// Defaults to 429 (Too Many Requests). Only applies to HTTP (non-gRPC) requests.
	HTTPStatus int `yaml:"http_status" json:"http_status,omitempty"`

	// HTTPBody is the response body for denied HTTP requests.
	// If empty, no body is returned.
	HTTPBody string `yaml:"http_body" json:"http_body,omitempty"`

	// GRPCStatusCode is the gRPC status code for denied gRPC requests.
	// Uses google.rpc.Code values. Defaults to 14 (UNAVAILABLE).
	GRPCStatusCode int `yaml:"grpc_status_code" json:"grpc_status_code,omitempty"`

	// GRPCStatusMessage is the gRPC error message for denied gRPC requests.
	GRPCStatusMessage string `yaml:"grpc_status_message" json:"grpc_status_message,omitempty"`

	// ResponseHeadersToAdd specifies headers to add to deny responses.
	ResponseHeadersToAdd map[string]string `yaml:"response_headers_to_add" json:"response_headers_to_add,omitempty"`
}

// PolicyConfig defines a rate limiting policy in YAML.
type PolicyConfig struct {
	DomainPattern    string `yaml:"domain_pattern"`
	BucketKeyPattern string `yaml:"bucket_key_pattern"`

	// TokensPerFill is the number of tokens added per reporting interval for the
	// token bucket strategy. Maps directly to TokenBucket.tokens_per_fill.
	TokensPerFill uint64 `yaml:"tokens_per_fill"`

	// RPS is deprecated: use TokensPerFill instead. The name was misleading because
	// the value represents tokens per reporting interval, not requests per second.
	RPS uint64 `yaml:"rps"`

	// AssignmentTTL is the TTL sent to clients with each quota assignment.
	// When omitted (nil), the default TTL (reporting_interval * 2) is used.
	// When set to "0s", the assignment expires immediately per the RLQS spec.
	AssignmentTTL *Duration `yaml:"assignment_ttl"`

	// Strategy selects the rate limiting strategy type.
	// Supported values: "token_bucket" (default), "requests_per_time_unit", "deny", "allow".
	// When "deny", a BlanketRule DENY_ALL is applied.
	// When "allow", a BlanketRule ALLOW_ALL is applied.
	// When "requests_per_time_unit", uses RequestsPerTimeUnit and TimeUnit fields.
	Strategy string `yaml:"strategy"`

	// RequestsPerTimeUnit is the number of requests allowed per time unit.
	// Only used when Strategy is "requests_per_time_unit".
	RequestsPerTimeUnit uint64 `yaml:"requests_per_time_unit"`

	// TimeUnit is the time unit for the requests_per_time_unit strategy.
	// Accepted values: "second", "minute", "hour", "day", "month", "year".
	// Only used when Strategy is "requests_per_time_unit".
	TimeUnit string `yaml:"time_unit"`

	// DenyResponse customizes the response returned to clients when requests are denied.
	DenyResponse *DenyResponseConfig `yaml:"deny_response" json:"deny_response,omitempty"`
}

// resolveDeprecated merges deprecated field names into their replacements and
// applies defaults. Call after YAML unmarshaling and before env var overrides.
func (e *EngineConfig) resolveDeprecated() {
	// DefaultRPS → DefaultTokensPerFill (prefer new name if both set)
	if e.DefaultTokensPerFill == 0 && e.DefaultRPS > 0 {
		e.DefaultTokensPerFill = e.DefaultRPS
	}
	if e.DefaultTokensPerFill == 0 {
		e.DefaultTokensPerFill = 100 // default
	}

	// Per-policy: RPS → TokensPerFill
	for i := range e.Policies {
		if e.Policies[i].TokensPerFill == 0 && e.Policies[i].RPS > 0 {
			e.Policies[i].TokensPerFill = e.Policies[i].RPS
		}
	}
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			GRPCAddr:              ":18081",
			MetricsAddr:           ":9090",
			MaxConcurrentStreams:  1000,
			MaxBucketsPerStream:   100,
			MaxReportsPerMessage:  1000,
			EngineTimeout:         Duration{5 * time.Second},
			MaxBucketEntries:      100,
			MaxBucketKeyLen:       256,
			MaxBucketValueLen:     1024,
			MaxMetricDomains:      100,
			KeepaliveMaxIdleTime:  Duration{5 * time.Minute},
			KeepalivePingInterval: Duration{1 * time.Minute},
			KeepalivePingTimeout:  Duration{20 * time.Second},
		},
		Engine: EngineConfig{
			ReportingInterval: Duration{10 * time.Second},
		},
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	// Resolve deprecated field names: prefer new names, fall back to deprecated.
	cfg.Engine.resolveDeprecated()

	if v := os.Getenv("RLQS_GRPC_ADDR"); v != "" {
		cfg.Server.GRPCAddr = v
	}
	if v := os.Getenv("RLQS_METRICS_ADDR"); v != "" {
		cfg.Server.MetricsAddr = v
	}
	if v := os.Getenv("RLQS_DEFAULT_TOKENS_PER_FILL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.Engine.DefaultTokensPerFill = n
		}
	} else if v := os.Getenv("RLQS_DEFAULT_RPS"); v != "" {
		// Deprecated env var: use RLQS_DEFAULT_TOKENS_PER_FILL instead.
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.Engine.DefaultTokensPerFill = n
		}
	}
	if v := os.Getenv("RLQS_REPORTING_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Engine.ReportingInterval = Duration{d}
		}
	}
	if v := os.Getenv("RLQS_TLS_CERT_FILE"); v != "" {
		cfg.Server.TLS.CertFile = v
	}
	if v := os.Getenv("RLQS_TLS_KEY_FILE"); v != "" {
		cfg.Server.TLS.KeyFile = v
	}
	if v := os.Getenv("RLQS_TLS_CA_FILE"); v != "" {
		cfg.Server.TLS.CAFile = v
	}
	if v := os.Getenv("RLQS_STORAGE_TYPE"); v != "" {
		cfg.Storage.Type = v
	}
	if v := os.Getenv("RLQS_STORAGE_REDIS_ADDR"); v != "" {
		cfg.Storage.Redis.Addr = v
	}
	if v := os.Getenv("RLQS_STORAGE_REDIS_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Storage.Redis.PoolSize = n
		}
	}
	if v := os.Getenv("RLQS_TRACING_ENABLED"); v != "" {
		cfg.Tracing.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("RLQS_TRACING_ENDPOINT"); v != "" {
		cfg.Tracing.Endpoint = v
	}
	if v := os.Getenv("RLQS_TRACING_INSECURE"); v != "" {
		cfg.Tracing.Insecure = v == "true" || v == "1"
	}

	return cfg, nil
}
