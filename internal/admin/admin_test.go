package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHandler(streams []StreamInfo, store storage.BucketStore) *Handler {
	return New(func() []StreamInfo { return streams }, store, &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":18081", MetricsAddr: ":9090"},
		Engine: config.EngineConfig{DefaultTokensPerFill: 100},
	})
}

func TestHealthz(t *testing.T) {
	h := newTestHandler(nil, storage.NewMemoryStorage())
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
}

func TestReadyz(t *testing.T) {
	h := newTestHandler(nil, storage.NewMemoryStorage())
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ready", body["status"])
}

func TestDebugStreams(t *testing.T) {
	streams := []StreamInfo{
		{Domain: "example.com", SubscriptionCount: 5},
		{Domain: "test.io", SubscriptionCount: 2},
	}
	h := newTestHandler(streams, storage.NewMemoryStorage())
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/streams", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var body []StreamInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body, 2)
	assert.Equal(t, "example.com", body[0].Domain)
	assert.Equal(t, 5, body[0].SubscriptionCount)
}

func TestDebugStreamsEmpty(t *testing.T) {
	h := newTestHandler(nil, storage.NewMemoryStorage())
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/streams", nil))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDebugBuckets(t *testing.T) {
	store := storage.NewMemoryStorage()
	// Add some domain-scoped buckets.
	store.Update(storage.DomainScopedKey("myapp", "bucket1"), 10, 2)
	store.Update(storage.DomainScopedKey("myapp", "bucket2"), 5, 1)
	store.Update(storage.DomainScopedKey("other", "bucket3"), 8, 0)

	h := newTestHandler(nil, store)
	mux := http.NewServeMux()
	h.Register(mux)

	t.Run("all buckets", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/buckets", nil))
		assert.Equal(t, http.StatusOK, w.Code)

		var body []map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Len(t, body, 3)
	})

	t.Run("filter by domain", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/buckets?domain=myapp", nil))
		assert.Equal(t, http.StatusOK, w.Code)

		var body []map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Len(t, body, 2)
	})

	t.Run("empty domain", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/buckets?domain=nonexistent", nil))
		assert.Equal(t, http.StatusOK, w.Code)

		var body []map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Empty(t, body)
	})
}

func TestDebugConfig(t *testing.T) {
	h := newTestHandler(nil, storage.NewMemoryStorage())
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/config", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	// Response is now wrapped: {"config": {...}, "pending_restart_changes": [...]}
	assert.Contains(t, body, "config")
	cfgMap, ok := body["config"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, cfgMap, "Server")
	assert.Contains(t, cfgMap, "Engine")
}

func TestDebugConfigWithPendingRestartChanges(t *testing.T) {
	h := newTestHandler(nil, storage.NewMemoryStorage())
	h.SetPendingRestartChanges([]config.ConfigChange{
		{Section: "server", Scope: config.ScopeRequiresRestart},
		{Section: "storage", Scope: config.ScopeRequiresRestart},
	})

	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/config", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body, "config")
	assert.Contains(t, body, "pending_restart_changes")

	pending, ok := body["pending_restart_changes"].([]any)
	require.True(t, ok)
	assert.Len(t, pending, 2)

	first, ok := pending[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "server", first["section"])
	assert.Equal(t, "requires-restart", first["scope"])
}

func TestDebugConfigNoPendingChanges(t *testing.T) {
	h := newTestHandler(nil, storage.NewMemoryStorage())
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/config", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	// When no pending changes, the field should be omitted (omitempty).
	_, hasPending := body["pending_restart_changes"]
	assert.False(t, hasPending)
}
