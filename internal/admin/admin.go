package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/storage"
)

// StreamInfo describes the state of a single active gRPC stream.
type StreamInfo struct {
	Domain            string `json:"domain"`
	SubscriptionCount int    `json:"subscription_count"`
}

// BucketLister is an optional interface for stores that support listing all buckets.
type BucketLister interface {
	All() map[storage.BucketKey]storage.UsageState
}

// Handler serves admin and debug HTTP endpoints.
type Handler struct {
	streamStats func() []StreamInfo
	store       storage.BucketStore
	cfg         atomic.Pointer[config.Config]
	token       string // bearer token for /debug/* auth; empty = no auth
}

// New creates an admin handler.
func New(streamStats func() []StreamInfo, store storage.BucketStore, cfg *config.Config) *Handler {
	h := &Handler{
		streamStats: streamStats,
		store:       store,
		token:       cfg.Server.AdminToken,
	}
	h.cfg.Store(cfg)
	return h
}

// UpdateConfig atomically replaces the config used by /debug/config.
func (h *Handler) UpdateConfig(cfg *config.Config) {
	h.cfg.Store(cfg)
}

// Register adds admin routes to the provided mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", h.handleHealthz)
	mux.HandleFunc("/readyz", h.handleReadyz)
	mux.HandleFunc("/debug/streams", h.requireAuth(h.handleDebugStreams))
	mux.HandleFunc("/debug/buckets", h.requireAuth(h.handleDebugBuckets))
	mux.HandleFunc("/debug/config", h.requireAuth(h.handleDebugConfig))
}

// requireAuth wraps a handler to require bearer token authentication
// when AdminToken is configured. If no token is configured, the handler
// is called directly.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.token != "" {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") || auth[7:] != h.token {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

func (h *Handler) handleDebugStreams(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.streamStats())
}

func (h *Handler) handleDebugBuckets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	lister, ok := h.store.(BucketLister)
	if !ok {
		http.Error(w, `{"error":"bucket listing not supported by storage backend"}`, http.StatusNotImplemented)
		return
	}

	domain := r.URL.Query().Get("domain")
	all := lister.All()

	type bucketEntry struct {
		Key     string `json:"key"`
		Allowed uint64 `json:"allowed"`
		Denied  uint64 `json:"denied"`
	}

	var result []bucketEntry
	for k, v := range all {
		keyStr := string(k)
		if domain != "" {
			// Domain-scoped keys have format: "domain\x1fbucketKey"
			prefix := domain + "\x1f"
			if len(keyStr) <= len(prefix) || keyStr[:len(prefix)] != prefix {
				continue
			}
			keyStr = keyStr[len(prefix):]
		}
		result = append(result, bucketEntry{
			Key:     keyStr,
			Allowed: v.Allowed,
			Denied:  v.Denied,
		})
	}

	if result == nil {
		result = []bucketEntry{}
	}
	_ = json.NewEncoder(w).Encode(result)
}

const redacted = "[REDACTED]"

func (h *Handler) handleDebugConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(RedactConfig(h.cfg.Load()))
}

// RedactConfig returns a copy of cfg with sensitive fields replaced.
func RedactConfig(cfg *config.Config) config.Config {
	out := *cfg

	// Redact TLS paths
	if out.Server.TLS.CertFile != "" {
		out.Server.TLS.CertFile = redacted
	}
	if out.Server.TLS.KeyFile != "" {
		out.Server.TLS.KeyFile = redacted
	}
	if out.Server.TLS.CAFile != "" {
		out.Server.TLS.CAFile = redacted
	}

	// Redact Redis address
	if out.Storage.Redis.Addr != "" {
		out.Storage.Redis.Addr = redacted
	}

	// Redact admin token
	if out.Server.AdminToken != "" {
		out.Server.AdminToken = redacted
	}

	// Redact tracing endpoint
	if out.Tracing.Endpoint != "" {
		out.Tracing.Endpoint = redacted
	}

	return out
}
