package config

import (
	"context"
	"os"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

func TestWatcherSendsConfigOnChange(t *testing.T) {
	// Write initial config file.
	f, err := os.CreateTemp("", "rlqs-watcher-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString("engine:\n  default_rps: 100\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	logger := zaptest.NewLogger(t)
	w := NewWatcher(f.Name(), logger)
	w.debounce = 50 * time.Millisecond // speed up for test

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Give the watcher time to register.
	time.Sleep(50 * time.Millisecond)

	// Modify the file.
	if err := os.WriteFile(f.Name(), []byte("engine:\n  default_rps: 200\n"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case cfg := <-ch:
		if cfg.Engine.DefaultRPS != 200 {
			t.Fatalf("expected default_rps=200, got %d", cfg.Engine.DefaultRPS)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for config reload")
	}
}

func TestWatcherIgnoresInvalidConfig(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-watcher-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString("engine:\n  default_rps: 100\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	logger := zaptest.NewLogger(t)
	w := NewWatcher(f.Name(), logger)
	w.debounce = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	// Write invalid YAML — should not send on channel.
	if err := os.WriteFile(f.Name(), []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case cfg := <-ch:
		t.Fatalf("should not have received config, got %+v", cfg)
	case <-time.After(500 * time.Millisecond):
		// Expected: no config sent for invalid YAML.
	}
}

func TestWatcherContextCancellation(t *testing.T) {
	f, err := os.CreateTemp("", "rlqs-watcher-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString("engine:\n  default_rps: 100\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	logger := zaptest.NewLogger(t)
	w := NewWatcher(f.Name(), logger)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	// Channel should close after context is cancelled.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for channel close")
	}
}

func TestWatcherNonexistentFile(t *testing.T) {
	logger := zaptest.NewLogger(t)
	w := NewWatcher("/nonexistent/path/config.yaml", logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := w.Watch(ctx)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}
