package admin

import (
	"encoding/json"
	"net/http"

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
	cfg         *config.Config
}

// New creates an admin handler.
func New(streamStats func() []StreamInfo, store storage.BucketStore, cfg *config.Config) *Handler {
	return &Handler{
		streamStats: streamStats,
		store:       store,
		cfg:         cfg,
	}
}

// Register adds admin routes to the provided mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", h.handleHealthz)
	mux.HandleFunc("/readyz", h.handleReadyz)
	mux.HandleFunc("/debug/streams", h.handleDebugStreams)
	mux.HandleFunc("/debug/buckets", h.handleDebugBuckets)
	mux.HandleFunc("/debug/config", h.handleDebugConfig)
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

func (h *Handler) handleDebugConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.cfg)
}
