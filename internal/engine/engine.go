package engine

import (
	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/storage"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Engine evaluates usage reports and produces quota assignments.
type Engine struct {
	cfg   config.EngineConfig
	store storage.Storage
}

func New(cfg config.EngineConfig, store storage.Storage) *Engine {
	return &Engine{cfg: cfg, store: store}
}

// BucketKeyFromID produces a stable string key from a BucketId.
// Delegates to storage.BucketKeyFromProto for a single canonical format.
func BucketKeyFromID(id *rlqspb.BucketId) storage.BucketKey {
	return storage.BucketKeyFromProto(id)
}

// ProcessReport processes a usage report and returns quota assignments.
func (e *Engine) ProcessReport(report *rlqspb.RateLimitQuotaUsageReports) *rlqspb.RateLimitQuotaResponse {
	resp := &rlqspb.RateLimitQuotaResponse{}

	for _, usage := range report.GetBucketQuotaUsages() {
		key := BucketKeyFromID(usage.GetBucketId())
		e.store.Update(key, usage.GetNumRequestsAllowed(), usage.GetNumRequestsDenied())

		action := &rlqspb.RateLimitQuotaResponse_BucketAction{
			BucketId: usage.GetBucketId(),
			BucketAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
				QuotaAssignmentAction: &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
					AssignmentTimeToLive: durationpb.New(e.cfg.ReportingInterval.Duration * 2),
					RateLimitStrategy: &typepb.RateLimitStrategy{
						Strategy: &typepb.RateLimitStrategy_RequestsPerTimeUnit_{
							RequestsPerTimeUnit: &typepb.RateLimitStrategy_RequestsPerTimeUnit{
								RequestsPerTimeUnit: e.cfg.DefaultRPS,
								TimeUnit:            typepb.RateLimitUnit_SECOND,
							},
						},
					},
				},
			},
		}
		resp.BucketAction = append(resp.GetBucketAction(), action)
	}

	return resp
}
