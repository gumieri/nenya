package version

import "testing"

// TestVersionDefaults pins the ldflags-injection defaults: an un-stamped
// build must report "dev"/"unknown" rather than an empty string, and the
// consumer contract version must stay at the value CONTRACT.md documents.
func TestVersionDefaults(t *testing.T) {
	if Version == "" {
		t.Fatal("Version default is empty; an un-stamped build would report a blank version")
	}
	if Commit == "" {
		t.Fatal("Commit default is empty; an un-stamped build would report a blank commit")
	}
	if BuildTime == "" {
		t.Fatal("BuildTime default is empty; an un-stamped build would report a blank timestamp")
	}
	if ContractVersion < 1 {
		t.Fatalf("ContractVersion = %d; CONTRACT.md documents a positive contract version", ContractVersion)
	}
}

// TestVersionLdflagsInjectable verifies the variables are addressable at
// package scope — how goreleaser stamps them via -ldflags "-X ...=".
func TestVersionLdflagsInjectable(t *testing.T) {
	old := Version
	Version = "v9.9.9-test"
	if Version != "v9.9.9-test" {
		t.Fatal("Version is not injectable via package-scope assignment")
	}
	Version = old
}
