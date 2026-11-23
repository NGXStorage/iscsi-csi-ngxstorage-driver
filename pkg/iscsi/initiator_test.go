package iscsi

import (
	"strings"
	"testing"
)

func TestSanitizeIQNAlias(t *testing.T) {
	if got := SanitizeIQNAlias("worker-0.ocp_2!"); got != "worker0ocp2" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeIQNAlias(""); got != "" {
		t.Fatalf("empty -> %q", got)
	}
}

func TestIQNAliasBoundedAndAlphanumeric(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		alias := IQNAlias("worker0.ocp.example.com")
		if len(alias) == 0 || len(alias) > 30 {
			t.Fatalf("alias length %d out of bounds: %q", len(alias), alias)
		}
		for _, r := range alias {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", r) {
				t.Fatalf("non-alphanumeric rune in %q", alias)
			}
		}
		if !strings.HasPrefix(alias, "worker0ocp") {
			t.Fatalf("sanitized prefix missing: %q", alias)
		}
		seen[alias] = true
	}
	if len(seen) < 2 {
		t.Fatal("random suffix must vary between calls")
	}
	if alias := IQNAlias("!!!"); !strings.HasPrefix(alias, "initiator") {
		t.Fatalf("empty base fallback: %q", alias)
	}
}
