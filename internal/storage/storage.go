package storage

import (
	"context"
	"sort"
	"strings"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
)

// BucketKey is a canonical string representation of a BucketId, used as a map key.
type BucketKey string

// BucketKeyFromProto produces a canonical, order-independent key from a BucketId proto.
func BucketKeyFromProto(id *rlqspb.BucketId) BucketKey {
	if id == nil {
		return ""
	}
	pairs := make([]string, 0, len(id.GetBucket()))
	for k, v := range id.GetBucket() {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return BucketKey(strings.Join(pairs, "\x00"))
}

// UsageReport captures a single bucket's usage data from a client report.
type UsageReport struct {
	BucketId           *rlqspb.BucketId
	NumRequestsAllowed uint64
	NumRequestsDenied  uint64
}

// BucketStore persists and retrieves per-bucket quota state.
type BucketStore interface {
	// RecordUsage stores a usage report for the given domain and bucket.
	RecordUsage(ctx context.Context, domain string, report UsageReport) error

	// RemoveBucket deletes all state for the given domain and bucket.
	RemoveBucket(ctx context.Context, domain string, key BucketKey) error
}
