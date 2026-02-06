package policy

import (
	"context"
	"fmt"
	"regexp"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/jukie/rlqs/internal/config"
	"github.com/jukie/rlqs/internal/storage"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Policy defines a rate limiting policy that can be matched against
// domains and bucket keys.
type Policy struct {
	// DomainPattern is a regex pattern to match against the domain.
	// Empty string matches all domains.
	DomainPattern string

	// BucketKeyPattern is a regex pattern to match against the bucket key.
	// Empty string matches all bucket keys.
	BucketKeyPattern string

	// Strategy is the rate limit strategy to apply to matching buckets.
	Strategy *typev3.RateLimitStrategy

	// AssignmentTTL is the TTL sent to clients with each quota assignment.
	AssignmentTTL time.Duration

	// DenyResponse holds optional deny response customization for this policy.
	DenyResponse *config.DenyResponseConfig

	// compiled regex patterns (pre-compiled during New())
	domainRegex    *regexp.Regexp
	bucketKeyRegex *regexp.Regexp
}

// compilePatterns pre-compiles regex patterns. Must be called before concurrent use.
func (p *Policy) compilePatterns() error {
	if p.DomainPattern != "" {
		r, err := regexp.Compile(p.DomainPattern)
		if err != nil {
			return fmt.Errorf("invalid domain pattern %q: %w", p.DomainPattern, err)
		}
		p.domainRegex = r
	}
	if p.BucketKeyPattern != "" {
		r, err := regexp.Compile(p.BucketKeyPattern)
		if err != nil {
			return fmt.Errorf("invalid bucket key pattern %q: %w", p.BucketKeyPattern, err)
		}
		p.bucketKeyRegex = r
	}
	return nil
}

// matches checks if this policy matches the given domain and bucket key.
// Regexes must be pre-compiled via compilePatterns before calling matches.
func (p *Policy) matches(domain string, bucketKey storage.BucketKey) bool {
	if p.domainRegex != nil && !p.domainRegex.MatchString(domain) {
		return false
	}
	if p.bucketKeyRegex != nil && !p.bucketKeyRegex.MatchString(string(bucketKey)) {
		return false
	}
	return true
}

// EngineConfig holds configuration for the Engine.
type EngineConfig struct {
	// Policies is a list of policies to match against, in priority order.
	// The first matching policy is used.
	Policies []Policy

	// DefaultPolicy is used if no policies match.
	DefaultPolicy Policy
}

// Engine implements the quota.Engine interface, selecting policies
// based on domain and bucket key patterns.
type Engine struct {
	cfg EngineConfig
}

// New creates an Engine with the given configuration. All regex patterns
// are pre-compiled to avoid data races during concurrent use.
func New(cfg EngineConfig) (*Engine, error) {
	for i := range cfg.Policies {
		if err := cfg.Policies[i].compilePatterns(); err != nil {
			return nil, fmt.Errorf("policy %d: %w", i, err)
		}
	}
	if err := cfg.DefaultPolicy.compilePatterns(); err != nil {
		return nil, fmt.Errorf("default policy: %w", err)
	}
	return &Engine{cfg: cfg}, nil
}

// ProcessUsage evaluates usage reports and returns quota assignment actions
// based on matched policies.
func (e *Engine) ProcessUsage(_ context.Context, domain string, reports []storage.UsageReport) ([]*rlqspb.RateLimitQuotaResponse_BucketAction, error) {
	if len(reports) == 0 {
		return nil, nil
	}

	actions := make([]*rlqspb.RateLimitQuotaResponse_BucketAction, 0, len(reports))
	for _, r := range reports {
		if r.BucketId == nil || len(r.BucketId.GetBucket()) == 0 {
			continue
		}

		bucketKey := storage.BucketKeyFromProto(r.BucketId)

		// Find first matching policy
		var matchedPolicy *Policy
		for i := range e.cfg.Policies {
			if e.cfg.Policies[i].matches(domain, bucketKey) {
				matchedPolicy = &e.cfg.Policies[i]
				break
			}
		}

		// Fallback to default policy if no match
		if matchedPolicy == nil {
			matchedPolicy = &e.cfg.DefaultPolicy
		}

		// Build assignment action
		assignment := &rlqspb.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{}
		if matchedPolicy.AssignmentTTL > 0 {
			assignment.AssignmentTimeToLive = durationpb.New(matchedPolicy.AssignmentTTL)
		}
		if matchedPolicy.Strategy != nil {
			assignment.RateLimitStrategy = matchedPolicy.Strategy
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
