package coord

import (
	"sort"
	"testing"
)

func TestLocalIdentityIsSortedAndDeterministic(t *testing.T) {
	a, b := LocalIdentity(), LocalIdentity()
	if a.CacheKey == "" || a.CacheKey != b.CacheKey {
		t.Fatalf("cache key not deterministic: %q vs %q", a.CacheKey, b.CacheKey)
	}
	if !sort.StringsAreSorted(a.Capabilities) {
		t.Fatalf("capabilities not sorted: %v", a.Capabilities)
	}
	seen := map[string]bool{}
	for _, c := range a.Capabilities {
		if seen[c] {
			t.Fatalf("duplicate capability %q", c)
		}
		seen[c] = true
	}
	// Callers must not be able to mutate the compiled-in list.
	a.Capabilities[0] = "mutated"
	if Capabilities()[0] == "mutated" {
		t.Fatal("Capabilities() returned the backing array")
	}
}

func TestCacheKeyTracksBuildAndCapabilities(t *testing.T) {
	base := NewIdentity("0.1.0", "abc", "0.1.0", []string{"b", "a"})
	if base.Capabilities[0] != "a" {
		t.Fatalf("NewIdentity did not sort: %v", base.Capabilities)
	}
	same := NewIdentity("0.1.0", "abc", "0.1.0", []string{"a", "b"})
	if base.CacheKey != same.CacheKey {
		t.Fatal("order of input capabilities changed the cache key")
	}
	if NewIdentity("0.1.0", "def", "0.1.0", []string{"a", "b"}).CacheKey == base.CacheKey {
		t.Fatal("different build, same cache key")
	}
	if NewIdentity("0.1.0", "abc", "0.1.0", []string{"a"}).CacheKey == base.CacheKey {
		t.Fatal("different capabilities, same cache key")
	}
}
