package metrics

import (
	"fmt"
	"regexp"
	"sync"
)

// UnknownDomain is the overflow label used for domains that don't match any
// configured policy pattern or when the unique-domain cap has been reached.
const UnknownDomain = "__unknown__"

// DomainClassifier maps raw domain strings to bounded Prometheus label values.
// Domains matching a configured policy pattern are passed through; all others
// collapse to UnknownDomain. A cap on unique domains provides a safety net
// against cardinality explosion even when patterns are broad.
type DomainClassifier struct {
	patterns   []*regexp.Regexp
	mu         sync.RWMutex
	known      map[string]bool
	maxDomains int
}

// NewDomainClassifier creates a classifier from domain regex pattern strings.
// maxDomains caps the total unique domain labels (safety net). If <= 0, defaults to 100.
// Empty pattern strings are skipped (they match everything, which defeats the purpose).
func NewDomainClassifier(patterns []string, maxDomains int) (*DomainClassifier, error) {
	if maxDomains <= 0 {
		maxDomains = 100
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		r, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid domain pattern %q: %w", p, err)
		}
		compiled = append(compiled, r)
	}
	return &DomainClassifier{
		patterns:   compiled,
		known:      make(map[string]bool),
		maxDomains: maxDomains,
	}, nil
}

// Classify returns domain if it matches a known pattern and the unique-domain
// cap has not been exceeded. Otherwise it returns UnknownDomain.
// When no patterns are configured, all domains are allowed up to the cap.
func (c *DomainClassifier) Classify(domain string) string {
	// Fast path: already approved.
	c.mu.RLock()
	if c.known[domain] {
		c.mu.RUnlock()
		return domain
	}
	c.mu.RUnlock()

	// Check if domain matches any pattern. No patterns → allow all (up to cap).
	matched := len(c.patterns) == 0
	if !matched {
		for _, p := range c.patterns {
			if p.MatchString(domain) {
				matched = true
				break
			}
		}
	}
	if !matched {
		return UnknownDomain
	}

	// Promote to write lock and admit if under cap.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known[domain] {
		return domain
	}
	if len(c.known) >= c.maxDomains {
		return UnknownDomain
	}
	c.known[domain] = true
	return domain
}
