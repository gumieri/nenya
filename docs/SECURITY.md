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

Nenya stores all authentication tokens (client token, provider API keys, API key tokens) in RAM-locked memory to prevent sensitive data from being written to disk:

- **mlock/mmap**: All tokens are allocated using `syscall.Mmap` with `syscall.Mlock`, keeping them in physical RAM and preventing swapping to disk
- **Read-only sealing**: After all tokens are stored, the memory region is locked to read-only via `syscall.Mprotect`. Any accidental write (e.g., buffer overflow, use-after-free) triggers an immediate `SIGSEGV`
- **Core dump prevention**: The systemd service unit sets `LimitCORE=0` to prevent crash-dump exposure. On macOS, core dumps are handled by the system's crash reporter (see platform notes below)
- **Zero-fill on destroy**: Memory is explicitly zeroed before release via `syscall.Munmap`. Sealed memory is temporarily toggled to writable for zeroing, then unmapped
- **Constant-time comparison**: Token comparison uses `subtle.ConstantTimeCompare` to prevent timing side-channel attacks
- **No string copies**: Tokens are stored as `[]byte` slices, not Go strings (avoids GC-promoted copies that are hard to erase)

### Secure Memory Default

Starting from version 0.1.0, `secure_memory_required` defaults to `true` in the configuration. If `mlock` is unavailable (e.g., missing `CAP_IPC_LOCK` or `LimitMEMLOCK` ulimit), the gateway fails to start.

To opt out (e.g., for development environments), set `"secure_memory_required": false` in the server config. This logs a warning and falls back to heap storage.

### Provider API Key Protection

Provider API keys are stored in the same mlock-protected memory as the client token. When a request requires authentication, the key is retrieved from secure memory, added to the outgoing HTTP header, and the temporary buffer is left for GC collection. This ensures provider keys never persist as Go strings in the heap.

### Auth Metrics

The gateway exposes Prometheus counters for authentication events:

| Metric | Labels | Description |
|--------|--------|-------------|
| `nenya_auth_success_total` | `type` (client_token, api_key), `key` (name) | Successful authentications |
| `nenya_auth_failure_total` | `type` (missing_header, client_token_mismatch, api_key_mismatch) | Failed authentication attempts |
| `nenya_auth_denials_total` | `reason` (agent_not_allowed, endpoint_not_allowed, key_disabled, key_expired) | RBAC authorization denials |

### Role-Based Access Control (RBAC)

Nenya enforces per-API key access controls via RBAC. API keys defined in `secrets.json` under `api_keys` support:

**Roles:**
- `admin` — Unrestricted access to all agents and endpoints (bypasses RBAC checks)
- `user` — Access to configured agents and all non-admin endpoints
- `read-only` — Read-only access: GET requests only (e.g., `/v1/models`, `/healthz`, `/statsz`, `/metrics`)

**Agent Scoping:**
- `allowed_agents` list restricts which agents the key can access
- Empty list grants access to all agents (backward compatible)
- Admin keys bypass agent restrictions

**Endpoint Restrictions:**
- `allowed_endpoints` list allows fine-grained HTTP method + path allowlisting (e.g., `GET /v1/models`, `POST /v1/chat/completions`)
- Overrides default role-based permissions when set
- Empty list uses role-based default permissions
- Admin keys bypass endpoint restrictions

**Example API key configuration:**
```json
{
  "api_keys": {
    "dev-user": {
      "name": "dev-user",
      "token": "nk-...",
      "roles": ["user"],
      "allowed_agents": ["build", "plan"],
      "allowed_endpoints": ["GET /v1/models", "POST /v1/chat/completions"],
      "expires_at": "2026-12-31T23:59:59Z",
      "enabled": true
    }
  }
}
```

See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md#api-keys-for-client-rbac) for full API key configuration details.

### Deployment Requirements

For secure memory to work properly, the process needs:

- **Linux**: `CAP_IPC_LOCK` capability or `LimitMEMLOCK=infinity` in systemd (see below)
- **macOS**: Default soft limit is 512KB per process. For typical token counts, run `ulimit -l unlimited` before starting, or use `"secure_memory_required": false` to opt out

Configure systemd with:

```ini
[Service]
LimitMEMLOCK=infinity
LimitCORE=0
```

Without `LimitMEMLOCK=infinity` (Linux) or sufficient mlock limit (macOS), `mlock` will fail and the gateway reports `ErrMLockFailure`. Without `LimitCORE=0`, a crash could dump locked memory to a core file on disk.

See `deploy/nenya.service` for a complete hardened unit file.

**macOS mlock Limits (512KB)**

On macOS, the default `RLIMIT_MEMLOCK` soft limit is 512KB per process for non-root users. If your token storage needs exceed this, you have two options:

1. **Increase the limit** (recommended for development):
   ```bash
   ulimit -l unlimited  # or a higher value like 8192 (8MB)
   ```

2. **Disable secure memory** (for testing only):
   ```json
   {
     "server": {
       "secure_memory_required": false
     }
   }
   ```

On Linux with systemd, `LimitMEMLOCK=infinity` in the service unit automatically grants the required capability.

**Platform-Specific Behavior with `secure_memory_required=true`**

| Platform | mlock unavailable | Gateway behavior |
|----------|------------------|------------------|
| Linux | Missing `CAP_IPC_LOCK` or `LimitMEMLOCK` | **Fails to start** with error, log points to `docs/SECURITY.md` |
| macOS | Default 512KB limit exceeded | **Fails to start** with error, log points to `docs/SECURITY.md` and suggests `ulimit -l unlimited` |

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
