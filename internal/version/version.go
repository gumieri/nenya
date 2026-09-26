package version

// Version is the semantic version of the build, set via ldflags.
var Version = "dev"

// Commit is the git commit hash of the build, set via ldflags.
var Commit = "unknown"

// BuildTime is the UTC timestamp of the build, set via ldflags.
var BuildTime = "unknown"

// ContractVersion is the version of the external consumer contract documented
// in CONTRACT.md. It is bumped only on breaking changes to the contract
// surface; see CONTRACT.md §2.
const ContractVersion = 1
