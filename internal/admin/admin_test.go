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
		Engine: config.EngineConfig{DefaultRPS: 100},
	})
}

func newAuthHandler(streams []StreamInfo, store storage.BucketStore, token string) *Handler {
	return New(func() []StreamInfo { return streams }, store, &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":18081", MetricsAddr: ":9090", AdminToken: token},
		Engine: config.EngineConfig{DefaultRPS: 100},
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
	// Verify config structure is present (Go struct fields are capitalized in JSON).
	assert.Contains(t, body, "Server")
	assert.Contains(t, body, "Engine")
}

func TestDebugConfigRedactsSensitiveFields(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			GRPCAddr:   ":18081",
			AdminToken: "secret-token",
			TLS: config.TLSConfig{
				CertFile: "/etc/ssl/cert.pem",
				KeyFile:  "/etc/ssl/key.pem",
				CAFile:   "/etc/ssl/ca.pem",
			},
		},
		Storage: config.StorageConfig{
			Type:  "redis",
			Redis: config.RedisConfig{Addr: "redis:6379"},
		},
		Tracing: config.TracingConfig{
			Endpoint: "localhost:4317",
		},
	}
	h := New(func() []StreamInfo { return nil }, storage.NewMemoryStorage(), cfg)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/debug/config", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	server := body["Server"].(map[string]any)
	tls := server["TLS"].(map[string]any)
	assert.Equal(t, "[REDACTED]", tls["CertFile"])
	assert.Equal(t, "[REDACTED]", tls["KeyFile"])
	assert.Equal(t, "[REDACTED]", tls["CAFile"])
	assert.Equal(t, "[REDACTED]", server["AdminToken"])

	stor := body["Storage"].(map[string]any)
	redis := stor["Redis"].(map[string]any)
	assert.Equal(t, "[REDACTED]", redis["Addr"])

	tracing := body["Tracing"].(map[string]any)
	assert.Equal(t, "[REDACTED]", tracing["Endpoint"])
}

func TestDebugConfigEmptyFieldsNotRedacted(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":18081"},
	}
	h := New(func() []StreamInfo { return nil }, storage.NewMemoryStorage(), cfg)
	mux := http.NewServeMux()
	h.Register(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/config", nil))

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	server := body["Server"].(map[string]any)
	tls := server["TLS"].(map[string]any)
	assert.Equal(t, "", tls["CertFile"])
	assert.Equal(t, "", tls["KeyFile"])
	assert.Equal(t, "", tls["CAFile"])
}

func TestAuthRequired(t *testing.T) {
	h := newAuthHandler(nil, storage.NewMemoryStorage(), "my-secret")
	mux := http.NewServeMux()
	h.Register(mux)

	endpoints := []string{"/debug/streams", "/debug/buckets", "/debug/config"}

	t.Run("no token returns 401", func(t *testing.T) {
		for _, ep := range endpoints {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", ep, nil))
			assert.Equal(t, http.StatusUnauthorized, w.Code, "endpoint: %s", ep)
		}
	})

	t.Run("wrong token returns 401", func(t *testing.T) {
		for _, ep := range endpoints {
			req := httptest.NewRequest("GET", ep, nil)
			req.Header.Set("Authorization", "Bearer wrong-token")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			assert.Equal(t, http.StatusUnauthorized, w.Code, "endpoint: %s", ep)
		}
	})

	t.Run("correct token returns 200", func(t *testing.T) {
		for _, ep := range endpoints {
			req := httptest.NewRequest("GET", ep, nil)
			req.Header.Set("Authorization", "Bearer my-secret")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code, "endpoint: %s", ep)
		}
	})

	t.Run("healthz does not require auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("readyz does not require auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestNoAuthWhenTokenEmpty(t *testing.T) {
	h := newTestHandler(nil, storage.NewMemoryStorage())
	mux := http.NewServeMux()
	h.Register(mux)

	// Debug endpoints should be accessible without auth when no token is configured.
	endpoints := []string{"/debug/streams", "/debug/buckets", "/debug/config"}
	for _, ep := range endpoints {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", ep, nil))
		assert.Equal(t, http.StatusOK, w.Code, "endpoint: %s", ep)
	}
}

func TestUpdateConfig(t *testing.T) {
	cfg1 := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":18081", MetricsAddr: ":9090"},
		Engine: config.EngineConfig{DefaultRPS: 100},
	}
	cfg2 := &config.Config{
		Server: config.ServerConfig{GRPCAddr: ":18081", MetricsAddr: ":9090"},
		Engine: config.EngineConfig{DefaultRPS: 500},
	}

	h := New(func() []StreamInfo { return nil }, storage.NewMemoryStorage(), cfg1)
	mux := http.NewServeMux()
	h.Register(mux)

	// Initial config
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/config", nil))
	var body1 map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body1))
	engine1 := body1["Engine"].(map[string]any)
	assert.Equal(t, float64(100), engine1["DefaultRPS"])

	// Update config
	h.UpdateConfig(cfg2)

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/debug/config", nil))
	var body2 map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body2))
	engine2 := body2["Engine"].(map[string]any)
	assert.Equal(t, float64(500), engine2["DefaultRPS"])
}
