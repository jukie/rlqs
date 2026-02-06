package config

import (
	"testing"
	"time"
)

func TestDiffNoChanges(t *testing.T) {
	cfg := &Config{
		Server:  ServerConfig{GRPCAddr: ":18081"},
		Engine:  EngineConfig{DefaultRPS: 100},
		Storage: StorageConfig{Type: "memory"},
	}
	changes := Diff(cfg, cfg)
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %v", changes)
	}
}

func TestDiffEngineOnly(t *testing.T) {
	old := &Config{Engine: EngineConfig{DefaultRPS: 100}}
	new := &Config{Engine: EngineConfig{DefaultRPS: 200}}

	changes := Diff(old, new)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Section != "engine" {
		t.Fatalf("expected engine section, got %s", changes[0].Section)
	}
	if changes[0].Scope != ScopeHotReload {
		t.Fatalf("expected hot-reload scope, got %s", changes[0].Scope)
	}
}

func TestDiffServerRequiresRestart(t *testing.T) {
	old := &Config{Server: ServerConfig{GRPCAddr: ":18081"}}
	new := &Config{Server: ServerConfig{GRPCAddr: ":18082"}}

	changes := Diff(old, new)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Section != "server" {
		t.Fatalf("expected server section, got %s", changes[0].Section)
	}
	if changes[0].Scope != ScopeRequiresRestart {
		t.Fatalf("expected requires-restart scope, got %s", changes[0].Scope)
	}
}

func TestDiffStorageRequiresRestart(t *testing.T) {
	old := &Config{Storage: StorageConfig{Type: "memory"}}
	new := &Config{Storage: StorageConfig{Type: "redis"}}

	changes := Diff(old, new)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Section != "storage" || changes[0].Scope != ScopeRequiresRestart {
		t.Fatalf("expected storage/requires-restart, got %s/%s", changes[0].Section, changes[0].Scope)
	}
}

func TestDiffTracingRequiresRestart(t *testing.T) {
	old := &Config{Tracing: TracingConfig{Enabled: false}}
	new := &Config{Tracing: TracingConfig{Enabled: true, Endpoint: "localhost:4317"}}

	changes := Diff(old, new)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Section != "tracing" || changes[0].Scope != ScopeRequiresRestart {
		t.Fatalf("expected tracing/requires-restart, got %s/%s", changes[0].Section, changes[0].Scope)
	}
}

func TestDiffMultipleSections(t *testing.T) {
	old := &Config{
		Engine:  EngineConfig{DefaultRPS: 100},
		Server:  ServerConfig{GRPCAddr: ":18081"},
		Storage: StorageConfig{Type: "memory"},
		Tracing: TracingConfig{Enabled: false},
	}
	new := &Config{
		Engine:  EngineConfig{DefaultRPS: 200},
		Server:  ServerConfig{GRPCAddr: ":18082"},
		Storage: StorageConfig{Type: "redis"},
		Tracing: TracingConfig{Enabled: true},
	}

	changes := Diff(old, new)
	if len(changes) != 4 {
		t.Fatalf("expected 4 changes, got %d", len(changes))
	}

	hotReload := 0
	restart := 0
	for _, c := range changes {
		switch c.Scope {
		case ScopeHotReload:
			hotReload++
		case ScopeRequiresRestart:
			restart++
		}
	}
	if hotReload != 1 {
		t.Fatalf("expected 1 hot-reload change, got %d", hotReload)
	}
	if restart != 3 {
		t.Fatalf("expected 3 restart-required changes, got %d", restart)
	}
}

func TestDiffPolicyChange(t *testing.T) {
	old := &Config{
		Engine: EngineConfig{
			DefaultRPS:        100,
			ReportingInterval: Duration{10 * time.Second},
		},
	}
	new := &Config{
		Engine: EngineConfig{
			DefaultRPS:        100,
			ReportingInterval: Duration{10 * time.Second},
			Policies: []PolicyConfig{
				{DomainPattern: "*.example.com", RPS: 50},
			},
		},
	}

	changes := Diff(old, new)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Section != "engine" || changes[0].Scope != ScopeHotReload {
		t.Fatalf("expected engine/hot-reload, got %s/%s", changes[0].Section, changes[0].Scope)
	}
}
