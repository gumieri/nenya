// Package security provides secure memory management for sensitive data
// such as API tokens and cryptographic keys. It uses mmap'd, mlock'd pages
// to prevent memory from being swapped to disk, and provides constant-time
// comparison to resist timing side-channel attacks.
package security
