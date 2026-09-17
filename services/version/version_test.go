package version

import "testing"

func TestCurrent(t *testing.T) {
	got := Current()
	if got.Version != Version {
		t.Fatalf("Version = %q, want %q", got.Version, Version)
	}
	if got.APIVersion != APIVersion {
		t.Fatalf("APIVersion = %q, want %q", got.APIVersion, APIVersion)
	}
	if got.Codename != Codename {
		t.Fatalf("Codename = %q, want %q", got.Codename, Codename)
	}
}
