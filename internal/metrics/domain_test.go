package metrics

import (
	"fmt"
	"testing"
)

func TestClassifyKnownDomain(t *testing.T) {
	c, err := NewDomainClassifier([]string{`^api\.`, `^web\.`}, 100)
	if err != nil {
		t.Fatal(err)
	}

	if got := c.Classify("api.example.com"); got != "api.example.com" {
		t.Fatalf("expected api.example.com, got %s", got)
	}
	if got := c.Classify("web.frontend"); got != "web.frontend" {
		t.Fatalf("expected web.frontend, got %s", got)
	}
}

func TestClassifyUnknownDomain(t *testing.T) {
	c, err := NewDomainClassifier([]string{`^api\.`}, 100)
	if err != nil {
		t.Fatal(err)
	}

	if got := c.Classify("evil.attacker.com"); got != UnknownDomain {
		t.Fatalf("expected %s, got %s", UnknownDomain, got)
	}
}

func TestClassifyNoPatterns(t *testing.T) {
	c, err := NewDomainClassifier(nil, 3)
	if err != nil {
		t.Fatal(err)
	}

	// No patterns → all domains allowed up to cap.
	if got := c.Classify("a"); got != "a" {
		t.Fatalf("expected a, got %s", got)
	}
	if got := c.Classify("b"); got != "b" {
		t.Fatalf("expected b, got %s", got)
	}
	if got := c.Classify("c"); got != "c" {
		t.Fatalf("expected c, got %s", got)
	}
	// 4th domain exceeds cap.
	if got := c.Classify("d"); got != UnknownDomain {
		t.Fatalf("expected %s, got %s", UnknownDomain, got)
	}
	// Already-known domain still passes.
	if got := c.Classify("a"); got != "a" {
		t.Fatalf("expected a after cap, got %s", got)
	}
}

func TestClassifyCapExceeded(t *testing.T) {
	c, err := NewDomainClassifier([]string{`^d`}, 2)
	if err != nil {
		t.Fatal(err)
	}

	c.Classify("d1")
	c.Classify("d2")
	// 3rd matching domain should be capped.
	if got := c.Classify("d3"); got != UnknownDomain {
		t.Fatalf("expected %s for capped domain, got %s", UnknownDomain, got)
	}
}

func TestClassifyEmptyPatternsSkipped(t *testing.T) {
	// Empty patterns should be skipped (they match everything).
	c, err := NewDomainClassifier([]string{"", `^api\.`}, 100)
	if err != nil {
		t.Fatal(err)
	}

	// "random" doesn't match ^api\. and empty was skipped.
	if got := c.Classify("random"); got != UnknownDomain {
		t.Fatalf("expected %s, got %s", UnknownDomain, got)
	}
	if got := c.Classify("api.test"); got != "api.test" {
		t.Fatalf("expected api.test, got %s", got)
	}
}

func TestClassifyInvalidPattern(t *testing.T) {
	_, err := NewDomainClassifier([]string{`[invalid`}, 100)
	if err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
}

func TestClassifyDefaultMaxDomains(t *testing.T) {
	// maxDomains <= 0 should default to 100.
	c, err := NewDomainClassifier(nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Should accept up to 100 unique domains.
	for i := 0; i < 100; i++ {
		d := fmt.Sprintf("domain%d", i)
		if got := c.Classify(d); got != d {
			t.Fatalf("expected %s, got %s", d, got)
		}
	}
	// 101st should be capped.
	if got := c.Classify("domain100"); got != UnknownDomain {
		t.Fatalf("expected %s, got %s", UnknownDomain, got)
	}
}

func TestClassifyConcurrentAccess(t *testing.T) {
	c, err := NewDomainClassifier([]string{`^d`}, 1000)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			d := fmt.Sprintf("d%d", i)
			result := c.Classify(d)
			if result != d && result != UnknownDomain {
				t.Errorf("unexpected result %s for %s", result, d)
			}
		}(i)
	}
	for i := 0; i < 100; i++ {
		<-done
	}
}
