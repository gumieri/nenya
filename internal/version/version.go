package version

// Version is the semantic version of the build, set via ldflags.
var Version = "dev"

// Commit is the git commit hash of the build, set via ldflags.
var Commit = "unknown"

// BuildTime is the UTC timestamp of the build, set via ldflags.
var BuildTime = "unknown"
