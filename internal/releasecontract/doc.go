// Package releasecontract encodes section 7 of CONTRACT.md — the release
// artifact names, archive members, and integrity artifacts — and verifies
// produced release artifacts against it.
//
// The release pipeline runs TestDistArtifacts with NENYA_DIST_DIR pointing at a
// goreleaser dist/ directory so a release whose archives drift from the
// published contract fails before publishing. Drift guards in the package's
// tests additionally keep .goreleaser.yml, install.sh, and CONTRACT.md in sync.
package releasecontract
