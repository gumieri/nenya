# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest on `main` | Yes |
| Older releases | No |

## Reporting a Vulnerability

If you believe you have found a security vulnerability in Nenya, please report it responsibly.

**Do not** open a public GitHub issue for security vulnerabilities.

Instead, please email the maintainer directly:

- **Email**: `rgumieri@gmail.com` with the subject line `[Nenya Security] <brief description>`

Include as much information as possible:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Any suggested fix (optional)

## What to Expect

1. **Acknowledgment** within 48 hours
2. **Initial assessment** within 5 business days
3. **Resolution timeline** communicated based on severity
4. **CVE assignment** for critical vulnerabilities
5. **Coordinated disclosure** once a fix is released

## Security Architecture

### Secure Memory Storage

Nenya stores authentication tokens in RAM-locked memory to prevent sensitive data from being written to disk:

- **mlock/mmap**: Tokens are allocated using `syscall.Mmap` with `syscall.Mlock`, keeping them in physical RAM
- **Zero-fill on destroy**: Memory is zeroed before release via `syscall.Munmap`
- **Constant-time comparison**: Token comparison uses `subtle.ConstantTimeCompare` to prevent timing attacks
- **No string copies**: Tokens are stored as `[]byte` slices, not Go strings (avoids GC-promoted copies)

### Deployment Requirements

For secure memory to work properly, configure systemd with:

```ini
[Service]
LimitMEMLOCK=infinity
```

Without this setting, `mlock` will fail and the gateway will report `ErrMLockFailure`. See `deploy/nenya.service` for a complete hardened unit file.

### Rate Limiting

Authentication attempts are rate-limited per client IP to prevent brute-force attacks. The rate limiter is shared with the token-per-minute governor in `governance.ratelimit_max_rpm`.

## Scope

Security vulnerabilities include but are not limited to:

- Authentication/authorization bypasses
- Request smuggling or HTTP desync attacks
- Denial of service (resource exhaustion)
- Information disclosure (leaked secrets, headers, or internal state)
- SSRF or injection vulnerabilities

Issues outside scope (feature requests, bugs without security impact) should be reported via [GitHub Issues](https://github.com/gumieri/nenya/issues).
