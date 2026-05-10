# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- Token-budget trimming pipeline (`TrimPayload`) that drops oldest non-system messages and applies token-aware middle-out truncation when payload exceeds hard limit.
- Configurable `hard_limit_tokens` in `context` section to override the default `softLimit * 2` behavior.
- `TokenSnapshot` struct to capture per-request token usage (input, output, total).
- Metrics for tracking trimmed requests via `RecordTrimmedRequest`.
- Wire `TrimPayload` into the request transformation pipeline after `applyMaxTokens`.

### Changed
- `interceptContent` now checks against configurable hard limit and applies trimming before Bouncer interception when payload exceeds the hard limit.
- Updated `TransformDeps` to include `CountTokens` for token-aware truncation during request transformation.
- Updated `prepareAndSend` to pass `CountTokens` to `TransformDeps`.

### Fixed
- None.

## [0.1.0] - 2026-05-09
### Added
- Initial implementation of Nenya AI API Gateway/Proxy.
