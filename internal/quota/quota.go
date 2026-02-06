package quota

import (
	"context"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/jukie/rlqs/internal/storage"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Engine evaluates usage reports and produces bucket actions (assignments or abandons).
type Engine interface {
	// ProcessUsage evaluates a batch of bucket usage reports for the given domain
	// and returns the bucket actions to send back to the client. It may return nil
	// if no actions are needed.
	ProcessUsage(ctx context.Context, domain string, reports []storage.UsageReport) ([]*rlqspb.RateLimitQuotaResponse_BucketAction, error)
}

// DefaultEngineConfig holds configuration for the DefaultEngine.
type DefaultEngineConfig struct {
	// DefaultStrategy is the rate limit strategy assigned to new buckets.
	DefaultStrategy *typev3.RateLimitStrategy

	// AssignmentTTL is the TTL sent to clients with each quota assignment.
	AssignmentTTL time.Duration
}

// DefaultEngine implements the Engine interface, producing quota assignment actions
// for every usage report using a configurable default strategy.
type DefaultEngine struct {
	cfg DefaultEngineConfig
}

// NewDefaultEngine creates a DefaultEngine with the given configuration.
func NewDefaultEngine(cfg DefaultEngineConfig) *DefaultEngine {
	return &DefaultEngine{cfg: cfg}
}

// ProcessUsage evaluates usage reports and returns a QuotaAssignmentAction for each bucket.
func (e *DefaultEngine) ProcessUsage(_ context.Context, _ string, reports []storage.UsageReport) ([]*rlqspb.RateLimitQuotaResponse_BucketAction, error) {
	if len(reports) == 0 {
		return nil, nil
	}

	actions := make([]*rlqspb.RateLimitQuotaResponse_BucketAction, 0, len(reports))
	for _, r := range reports {
		if r.BucketId == nil || len(r.BucketId.GetBucket()) == 0 {
			continue
		}

		assignment := &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{}
		if e.cfg.AssignmentTTL > 0 {
			assignment.AssignmentTimeToLive = durationpb.New(e.cfg.AssignmentTTL)
		}
		if e.cfg.DefaultStrategy != nil {
			assignment.RateLimitStrategy = e.cfg.DefaultStrategy
		}

		action := &rlqspb.RateLimitQuotaResponse_BucketAction{
			BucketId: r.BucketId,
			BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
				QuotaAssignmentAction: assignment,
			},
		}
		actions = append(actions, action)
	}

	return actions, nil
}
