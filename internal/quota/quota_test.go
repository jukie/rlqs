package quota

import (
	"context"
	"testing"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/jukie/rlqs/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func durationPtr(d time.Duration) *time.Duration { return &d }

func TestDefaultEngine_ProcessUsageEmpty(t *testing.T) {
	eng := NewDefaultEngine(DefaultEngineConfig{})
	actions, err := eng.ProcessUsage(context.Background(), "test", nil)
	require.NoError(t, err)
	assert.Nil(t, actions)
}

func TestDefaultEngine_ProcessUsageAssignment(t *testing.T) {
	strategy := &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_RequestsPerTimeUnit_{
			RequestsPerTimeUnit: &typev3.RateLimitStrategy_RequestsPerTimeUnit{
				RequestsPerTimeUnit: 100,
				TimeUnit:            typev3.RateLimitUnit_SECOND,
			},
		},
	}

	eng := NewDefaultEngine(DefaultEngineConfig{
		DefaultStrategy: strategy,
		AssignmentTTL:   durationPtr(20 * time.Second),
	})

	reports := []storage.UsageReport{
		{
			BucketId:           &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}},
			NumRequestsAllowed: 42,
			NumRequestsDenied:  3,
		},
	}

	actions, err := eng.ProcessUsage(context.Background(), "test", reports)
	require.NoError(t, err)
	require.Len(t, actions, 1)

	action := actions[0]
	assert.Equal(t, "web", action.GetBucketId().GetBucket()["name"])

	qa := action.GetQuotaAssignmentAction()
	require.NotNil(t, qa)
	assert.Equal(t, 20*time.Second, qa.GetAssignmentTimeToLive().AsDuration())
	assert.Equal(t, uint64(100), qa.GetRateLimitStrategy().GetRequestsPerTimeUnit().GetRequestsPerTimeUnit())
}

func TestDefaultEngine_SkipsEmptyBucketId(t *testing.T) {
	eng := NewDefaultEngine(DefaultEngineConfig{
		DefaultStrategy: &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_BlanketRule_{
				BlanketRule: typev3.RateLimitStrategy_ALLOW_ALL,
			},
		},
	})

	reports := []storage.UsageReport{
		{BucketId: nil, NumRequestsAllowed: 10},
		{BucketId: &rlqspb.BucketId{}, NumRequestsAllowed: 10},
		{BucketId: &rlqspb.BucketId{Bucket: map[string]string{}}, NumRequestsAllowed: 10},
		{BucketId: &rlqspb.BucketId{Bucket: map[string]string{"name": "valid"}}, NumRequestsAllowed: 10},
	}

	actions, err := eng.ProcessUsage(context.Background(), "test", reports)
	require.NoError(t, err)
	assert.Len(t, actions, 1, "should only produce action for the valid bucket")
	assert.Equal(t, "valid", actions[0].GetBucketId().GetBucket()["name"])
}

func TestDefaultEngine_MultipleBuckets(t *testing.T) {
	eng := NewDefaultEngine(DefaultEngineConfig{
		DefaultStrategy: &typev3.RateLimitStrategy{
			Strategy: &typev3.RateLimitStrategy_BlanketRule_{
				BlanketRule: typev3.RateLimitStrategy_ALLOW_ALL,
			},
		},
		AssignmentTTL: durationPtr(30 * time.Second),
	})

	reports := []storage.UsageReport{
		{BucketId: &rlqspb.BucketId{Bucket: map[string]string{"ip": "10.0.0.1"}}, NumRequestsAllowed: 20},
		{BucketId: &rlqspb.BucketId{Bucket: map[string]string{"ip": "10.0.0.2"}}, NumRequestsAllowed: 30},
	}

	actions, err := eng.ProcessUsage(context.Background(), "test", reports)
	require.NoError(t, err)
	assert.Len(t, actions, 2)

	for _, a := range actions {
		assert.NotNil(t, a.GetQuotaAssignmentAction())
	}
}
