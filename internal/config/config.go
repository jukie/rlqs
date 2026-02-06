package config

import (
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server ServerConfig `yaml:"server"`
	Engine EngineConfig `yaml:"engine"`
}

type ServerConfig struct {
	GRPCAddr string `yaml:"grpc_addr"`
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
	DefaultRPS        uint64   `yaml:"default_rps"`
	ReportingInterval Duration `yaml:"reporting_interval"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			GRPCAddr: ":18081",
		},
		Engine: EngineConfig{
			DefaultRPS:        100,
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

	if v := os.Getenv("RLQS_GRPC_ADDR"); v != "" {
		cfg.Server.GRPCAddr = v
	}
	if v := os.Getenv("RLQS_DEFAULT_RPS"); v != "" {
		if rps, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.Engine.DefaultRPS = rps
		}
	}
	if v := os.Getenv("RLQS_REPORTING_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Engine.ReportingInterval = Duration{d}
		}
	}

	return cfg, nil
}
