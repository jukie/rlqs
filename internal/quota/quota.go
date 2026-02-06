package quota

import (
	"context"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"github.com/jukie/rlqs/internal/storage"
)

// Engine evaluates usage reports and produces bucket actions (assignments or abandons).
type Engine interface {
	// ProcessUsage evaluates a batch of bucket usage reports for the given domain
	// and returns the bucket actions to send back to the client. It may return nil
	// if no actions are needed.
	ProcessUsage(ctx context.Context, domain string, reports []storage.UsageReport) ([]*rlqspb.RateLimitQuotaResponse_BucketAction, error)
}
