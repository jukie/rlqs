package engine

import (
	"testing"
	"time"

	"rlqs/internal/config"
	"rlqs/internal/storage"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

func TestBucketKeyFromID(t *testing.T) {
	tests := []struct {
		name     string
		id       *rlqspb.BucketId
		expected storage.BucketKey
	}{
		{"nil", nil, ""},
		{"single", &rlqspb.BucketId{Bucket: map[string]string{"a": "1"}}, "a=1"},
		{"sorted", &rlqspb.BucketId{Bucket: map[string]string{"b": "2", "a": "1"}}, "a=1;b=2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BucketKeyFromID(tt.id)
			if got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestEngine_ProcessReport(t *testing.T) {
	store := storage.NewMemoryStorage()
	eng := New(config.EngineConfig{
		DefaultRPS:        50,
		ReportingInterval: config.Duration{Duration: 5 * time.Second},
	}, store)

	report := &rlqspb.RateLimitQuotaUsageReports{
		Domain: "test",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{"name": "b1"},
				},
				NumRequestsAllowed: 10,
				NumRequestsDenied:  2,
			},
		},
	}

	resp := eng.ProcessReport(report)

	if len(resp.GetBucketAction()) != 1 {
		t.Fatalf("expected 1 action, got %d", len(resp.GetBucketAction()))
	}

	action := resp.GetBucketAction()[0]
	qa := action.GetQuotaAssignmentAction()
	if qa == nil {
		t.Fatal("expected quota assignment action")
	}

	rps := qa.GetRateLimitStrategy().GetRequestsPerTimeUnit()
	if rps == nil {
		t.Fatal("expected requests per time unit strategy")
	}
	if rps.GetRequestsPerTimeUnit() != 50 {
		t.Fatalf("expected 50 rps, got %d", rps.GetRequestsPerTimeUnit())
	}
	if rps.GetTimeUnit() != typepb.RateLimitUnit_SECOND {
		t.Fatalf("expected SECOND, got %v", rps.GetTimeUnit())
	}

	state, ok := store.Get("name=b1")
	if !ok {
		t.Fatal("bucket not found in storage")
	}
	if state.Allowed != 10 || state.Denied != 2 {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestEngine_ProcessReportMultipleBuckets(t *testing.T) {
	store := storage.NewMemoryStorage()
	eng := New(config.EngineConfig{
		DefaultRPS:        100,
		ReportingInterval: config.Duration{Duration: 10 * time.Second},
	}, store)

	report := &rlqspb.RateLimitQuotaUsageReports{
		Domain: "prod",
		BucketQuotaUsages: []*rlqspb.RateLimitQuotaUsageReports_BucketQuotaUsage{
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"ip": "10.0.0.1"}},
				NumRequestsAllowed: 50,
			},
			{
				BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"ip": "10.0.0.2"}},
				NumRequestsAllowed: 30,
				NumRequestsDenied:  5,
			},
		},
	}

	resp := eng.ProcessReport(report)
	if len(resp.GetBucketAction()) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(resp.GetBucketAction()))
	}

	for _, a := range resp.GetBucketAction() {
		if a.GetQuotaAssignmentAction() == nil {
			t.Fatal("expected quota assignment for every bucket")
		}
	}
}
