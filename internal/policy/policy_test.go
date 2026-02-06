package policy

import (
	"context"
	"testing"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/jukie/rlqs/internal/storage"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestPolicyMatches(t *testing.T) {
	tests := []struct {
		name      string
		policy    Policy
		domain    string
		bucketKey storage.BucketKey
		want      bool
	}{
		{
			name: "empty patterns match all",
			policy: Policy{
				DomainPattern:    "",
				BucketKeyPattern: "",
			},
			domain:    "api.example.com",
			bucketKey: "bucket1",
			want:      true,
		},
		{
			name: "domain pattern matches",
			policy: Policy{
				DomainPattern:    "^api\\.",
				BucketKeyPattern: "",
			},
			domain:    "api.example.com",
			bucketKey: "bucket1",
			want:      true,
		},
		{
			name: "domain pattern does not match",
			policy: Policy{
				DomainPattern:    "^api\\.",
				BucketKeyPattern: "",
			},
			domain:    "internal.example.com",
			bucketKey: "bucket1",
			want:      false,
		},
		{
			name: "bucket key pattern matches",
			policy: Policy{
				DomainPattern:    "",
				BucketKeyPattern: "user",
			},
			domain:    "api.example.com",
			bucketKey: "user123",
			want:      true,
		},
		{
			name: "bucket key pattern does not match",
			policy: Policy{
				DomainPattern:    "",
				BucketKeyPattern: "admin",
			},
			domain:    "api.example.com",
			bucketKey: "user123",
			want:      false,
		},
		{
			name: "both patterns match",
			policy: Policy{
				DomainPattern:    "^api\\.",
				BucketKeyPattern: "user",
			},
			domain:    "api.example.com",
			bucketKey: "user123",
			want:      true,
		},
		{
			name: "domain matches but bucket key does not",
			policy: Policy{
				DomainPattern:    "^api\\.",
				BucketKeyPattern: "admin",
			},
			domain:    "api.example.com",
			bucketKey: "user123",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.matches(tt.domain, tt.bucketKey)
			if got != tt.want {
				t.Errorf("matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEngineProcessUsage(t *testing.T) {
	// Create test strategies
	strategy100 := &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_TokenBucket{
			TokenBucket: &typev3.TokenBucket{
				MaxTokens:     100,
				TokensPerFill: wrapperspb.UInt32(100),
				FillInterval:  durationpb.New(1 * time.Second),
			},
		},
	}

	strategy1000 := &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_TokenBucket{
			TokenBucket: &typev3.TokenBucket{
				MaxTokens:     1000,
				TokensPerFill: wrapperspb.UInt32(1000),
				FillInterval:  durationpb.New(1 * time.Second),
			},
		},
	}

	engine := New(EngineConfig{
		Policies: []Policy{
			{
				DomainPattern:    "^api\\.",
				BucketKeyPattern: "",
				Strategy:         strategy1000,
				AssignmentTTL:    20 * time.Second,
			},
		},
		DefaultPolicy: Policy{
			Strategy:      strategy100,
			AssignmentTTL: 10 * time.Second,
		},
	})

	t.Run("matches first policy", func(t *testing.T) {
		reports := []storage.UsageReport{
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{"name": "test"},
				},
				NumRequestsAllowed: 10,
				NumRequestsDenied:  0,
				TimeElapsed:        1 * time.Second,
			},
		}

		actions, err := engine.ProcessUsage(context.Background(), "api.example.com", reports)
		if err != nil {
			t.Fatalf("ProcessUsage() error = %v", err)
		}

		if len(actions) != 1 {
			t.Fatalf("got %d actions, want 1", len(actions))
		}

		action := actions[0].GetQuotaAssignmentAction()
		if action == nil {
			t.Fatal("expected QuotaAssignmentAction, got nil")
		}

		// Check that strategy1000 was applied
		tb := action.RateLimitStrategy.GetTokenBucket()
		if tb == nil {
			t.Fatal("expected TokenBucket strategy")
		}
		if tb.MaxTokens != 1000 {
			t.Errorf("got MaxTokens = %d, want 1000", tb.MaxTokens)
		}

		// Check TTL
		if action.AssignmentTimeToLive.AsDuration() != 20*time.Second {
			t.Errorf("got TTL = %v, want 20s", action.AssignmentTimeToLive.AsDuration())
		}
	})

	t.Run("falls back to default policy", func(t *testing.T) {
		reports := []storage.UsageReport{
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{"name": "test"},
				},
				NumRequestsAllowed: 10,
				NumRequestsDenied:  0,
				TimeElapsed:        1 * time.Second,
			},
		}

		actions, err := engine.ProcessUsage(context.Background(), "internal.example.com", reports)
		if err != nil {
			t.Fatalf("ProcessUsage() error = %v", err)
		}

		if len(actions) != 1 {
			t.Fatalf("got %d actions, want 1", len(actions))
		}

		action := actions[0].GetQuotaAssignmentAction()
		if action == nil {
			t.Fatal("expected QuotaAssignmentAction, got nil")
		}

		// Check that strategy100 (default) was applied
		tb := action.RateLimitStrategy.GetTokenBucket()
		if tb == nil {
			t.Fatal("expected TokenBucket strategy")
		}
		if tb.MaxTokens != 100 {
			t.Errorf("got MaxTokens = %d, want 100", tb.MaxTokens)
		}

		// Check TTL
		if action.AssignmentTimeToLive.AsDuration() != 10*time.Second {
			t.Errorf("got TTL = %v, want 10s", action.AssignmentTimeToLive.AsDuration())
		}
	})

	t.Run("handles empty reports", func(t *testing.T) {
		actions, err := engine.ProcessUsage(context.Background(), "api.example.com", nil)
		if err != nil {
			t.Fatalf("ProcessUsage() error = %v", err)
		}
		if actions != nil {
			t.Errorf("got %v, want nil", actions)
		}
	})

	t.Run("skips invalid bucket IDs", func(t *testing.T) {
		reports := []storage.UsageReport{
			{
				BucketId:           nil,
				NumRequestsAllowed: 10,
			},
			{
				BucketId: &rlqspb.BucketId{
					Bucket: map[string]string{},
				},
				NumRequestsAllowed: 10,
			},
		}

		actions, err := engine.ProcessUsage(context.Background(), "api.example.com", reports)
		if err != nil {
			t.Fatalf("ProcessUsage() error = %v", err)
		}
		if len(actions) != 0 {
			t.Errorf("got %d actions, want 0", len(actions))
		}
	})
}
