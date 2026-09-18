package main

import (
	"strings"
	"testing"
)

// The cloud-storage form builds an rclone command line from operator input, so
// these tests pin the pieces that keep that argv a closed set.

func TestValidateRemoteName(t *testing.T) {
	for _, name := range []string{"my-cloud", "onyx_backups", "b2.prod", "a", "AWS2026"} {
		if err := validateRemoteName(name); err != nil {
			t.Errorf("validateRemoteName(%q) = %v, want nil", name, err)
		}
	}
	// A name becomes an argv element: ':' and '/' are rclone separators, a
	// leading '-' would be read as a flag, and whitespace or newlines must
	// never reach the command line.
	for _, name := range []string{"", "-flag", "a:b", "a/b", "a b", "a\nb", "a$b", strings.Repeat("x", 65)} {
		if err := validateRemoteName(name); err == nil {
			t.Errorf("validateRemoteName(%q) = nil, want an error", name)
		}
	}
}

func TestProviderCatalogIsSelfConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range rcloneProviders() {
		if p.Type == "" || p.Label == "" {
			t.Errorf("provider %+v is missing a type or label", p)
		}
		if seen[p.Type] {
			t.Errorf("provider %q is listed twice", p.Type)
		}
		seen[p.Type] = true
		fields := map[string]bool{}
		for _, f := range p.Fields {
			fields[f] = true
		}
		for _, required := range p.Required {
			if !fields[required] {
				t.Errorf("provider %q requires %q, which is not one of its fields", p.Type, required)
			}
		}
		if p.OAuth && len(p.Required) > 0 {
			t.Errorf("provider %q is OAuth but demands stored credentials", p.Type)
		}
	}
}

func TestProviderByType(t *testing.T) {
	if p, ok := providerByType("S3"); !ok || p.Type != "s3" {
		t.Fatalf("providerByType(\"S3\") = %+v, %v; want the s3 provider", p, ok)
	}
	if _, ok := providerByType("  b2  "); !ok {
		t.Error("providerByType should trim whitespace")
	}
	if _, ok := providerByType("nfs-share"); ok {
		t.Error("unknown backends must be rejected rather than guessed")
	}
}

func TestLastLinesKeepsTheSummary(t *testing.T) {
	output := "a\nb\nc\nd\ne\nf\n"
	if got := lastLines(output, 2); got != "e\nf" {
		t.Errorf("lastLines = %q, want %q", got, "e\nf")
	}
	if got := lastLines("only", 5); got != "only" {
		t.Errorf("lastLines = %q, want %q", got, "only")
	}
}

func TestContainsString(t *testing.T) {
	if !containsString([]string{"a", "b"}, "b") {
		t.Error("containsString missed an existing entry")
	}
	if containsString([]string{"a"}, "b") || containsString(nil, "b") {
		t.Error("containsString found a missing entry")
	}
}
