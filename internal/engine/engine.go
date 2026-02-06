package engine

import (
	"fmt"
	"sort"
	"strings"

	"rlqs/internal/config"
	"rlqs/internal/storage"

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
func BucketKeyFromID(id *rlqspb.BucketId) storage.BucketKey {
	if id == nil {
		return ""
	}
	keys := make([]string, 0, len(id.GetBucket()))
	for k := range id.GetBucket() {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(';')
		}
		fmt.Fprintf(&sb, "%s=%s", k, id.GetBucket()[k])
	}
	return storage.BucketKey(sb.String())
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
