# Security Policy

## Supported Versions

| Version          | Supported |
| ---------------- | --------- |
| Latest on `main` | Yes       |
| Older releases   | No        |

## Reporting a Vulnerability

If you believe you have found a security vulnerability in Nenya, please report it responsibly.

**Do not** open a public GitHub issue for security vulnerabilities.

**Preferred: GitHub Private Vulnerability Reporting.** Use
[Report a vulnerability](https://github.com/gumieri/nenya/security/advisories/new)
on this repository — reports stay private, integrate with CodeQL, and feed
the coordinated-disclosure advisory/CVE workflow directly. Please include:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Any suggested fix (optional)

**Alternative: email** the maintainer at `rgumieri@gmail.com` with the
subject `[Nenya Security] <brief description>` if you cannot use GitHub.

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
- **Zero-fill on destroy**: Memory is explicitly zeroed before release via `syscall.Munmap`. Sealed memory is temporarily toggled to writable for zeroing, then unmapped. A centralized `util.ZeroBytes()` helper in `internal/util/zero.go` provides a reusable zeroing primitive for other subsystems
- **Constant-time comparison**: Token comparison uses `subtle.ConstantTimeCompare` to prevent timing side-channel attacks
- **No string copies**: Tokens are stored as `[]byte` slices, not Go strings (avoids GC-promoted copies that are hard to erase)

### Secure Memory Default

Starting from version 0.1.0, `secure_memory_required` defaults to `true` in the configuration. If `mlock` is unavailable (e.g., missing `CAP_IPC_LOCK` or `LimitMEMLOCK` ulimit), the gateway fails to start.

To opt out (e.g., for development environments), set `"secure_memory_required": false` in the server config. This logs a warning and falls back to heap storage.

### Provider API Key Protection

Provider API keys are stored in the same mlock-protected memory as the client token. When a request requires authentication, the key is retrieved from secure memory, added to the outgoing HTTP header, and the temporary buffer is left for GC collection. This ensures provider keys never persist as Go strings in the heap.

### Auth Metrics

The gateway exposes Prometheus counters for authentication events:

| Metric                     | Labels                                                                        | Description                    |
| -------------------------- | ----------------------------------------------------------------------------- | ------------------------------ |
| `nenya_auth_success_total` | `type` (client_token, api_key), `key_name` (key name, or `redacted`)          | Successful authentications     |
| `nenya_auth_failure_total` | `type` (missing_header, client_token_mismatch, api_key_mismatch)              | Failed authentication attempts |
| `nenya_auth_denials_total` | `key_name` (key name, or `redacted`; budget denials carry the agent name), `reason` (agent, endpoint, disabled, expired, rate_limited, key_budget, provider_budget, payload_too_large, invalid_body) | RBAC authorization denials     |

With `server.telemetry_unauthenticated` set, `key_name` labels are replaced
by the constant `redacted` so an unauthenticated `/metrics` scrape cannot
enumerate API key names (NENYA-131).

### Outbound Transport Policy (NENYA-137)

Every outbound HTTP transport (provider dispatches, model discovery,
billing quota fetches, MCP connections, local Ollama) carries an explicit
TLS 1.2 floor. `providers.<name>.ca_bundle` / `network.ca_bundle` append a
private CA to the system roots — private CAs grant trust without disabling
system trust. `providers.<name>.proxy_url` / `network.proxy_url` route
dispatches through an http/https/socks5/socks5h egress proxy; when unset, the
standard `HTTPS_PROXY`/`NO_PROXY` environment variables apply. CA paths and
proxy schemes are validated at startup and the startup gate fails on an
invalid policy; only direct-built constructions (tests) degrade to system
defaults with an error log rather than skipping validation silently.

### Telemetry Endpoints

`/statsz` and `/metrics` require authentication (any role — `read-only`
suffices for monitoring scrapers) and `/statsz` reports per-key usage keyed by
key name, so they must not be exposed unauthenticated (NENYA-131). The default
listen address is loopback (`127.0.0.1:8080`) for the same reason. Operators
who need the legacy behavior (e.g. a scraping daemon on a loopback-only
listener) can set `server.telemetry_unauthenticated: true` — in that mode
`/statsz` omits the per-key section entirely and auth metrics replace
`key_name` label values with `redacted`, so key names never leave the process
(a startup warning fires if the flag is combined with a non-loopback
listener). `/healthz` is always unauthenticated by design.

### Advisory Judgment Layer and Egress Screen

Nenya's deterministic output controls (ExfilGuard URL policy, canary tripwire,
inbound entropy redaction) are exact and cheap, but a model can exfiltrate
through **encoded or paraphrased** content that no deterministic rule catches.
The advisory judgment layer adds a bounded LLM adjudication for output that has
**already tripped a deterministic egress signal** — it never scans the general
response, only the flagged surface:

- **Threat model.** Deterministic signals are high-recall/low-precision: a
  flagged markdown link, a canary echo, a redaction event, or suspicious MCP
  tool arguments may be benign or a real exfil attempt. The `egress_screen`
  judgment adjudicates the flagged content at the **buffered-response** and
  **MCP tool-args** checkpoints only; streaming deltas and the MCP buffered
  replay are not screened (placement rule).
- **Strengthen-only semantics.** A judgment verdict can only **strengthen** the
  outcome: under `action: "strict"` an `exfil` verdict raises a structured
  `exfil_detected` block (the request is refused); under the default `log`
  action it is recorded as `nenya_exfil_detections_total{reason="llm_screen"}`
  alongside the deterministic detection. A non-verdict (engine failure, timeout,
  budget exhaustion, malformed reply, truncated excerpt) leaves the
  deterministic decision unchanged. The advisory layer can never clear a
  deterministic block or weaken a tier-1 verdict.
- **Budget caps.** Each adjudication is bounded by `max_bytes` (excerpt cap),
  `timeout_seconds` (total chain bound), and a **per-request budget** so a
  request with many flagged surfaces produces at most one judgment call. The
  judgment traffic is an egress event in its own right, so the excerpt is
  size-capped before it leaves the gateway.

All of this rests on the same engine-agnostic contract design as the rest of
the layer (see [ARCHITECTURE.md](ARCHITECTURE.md#advisory-judgment-layer) and
[CONFIGURATION.md](CONFIGURATION.md#governance)).

### Role-Based Access Control (RBAC)

Nenya enforces per-API key access controls via RBAC. API keys defined in `secrets.json` under `api_keys` support:

**Roles:**

- `admin` — Unrestricted access to all agents and endpoints (bypasses RBAC checks)
- `user` — Access to configured agents and all non-admin endpoints
- `read-only` — Monitoring role: GET requests on `/v1/models`, `/healthz`, `/statsz`, `/metrics` only (path-scoped since NENYA-131 — other GET surfaces such as `/proxy/*` and `/debug/pprof` require `user` or `admin`). When combined with other roles on one key, the read-only scoping wins (deny-wins)

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

| Platform | mlock unavailable                        | Gateway behavior                                                                                   |
| -------- | ---------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Linux    | Missing `CAP_IPC_LOCK` or `LimitMEMLOCK` | **Fails to start** with error, log points to `docs/SECURITY.md`                                    |
| macOS    | Default 512KB limit exceeded             | **Fails to start** with error, log points to `docs/SECURITY.md` and suggests `ulimit -l unlimited` |

### Rate Limiting

Authentication attempts are rate-limited per client IP to prevent brute-force attacks. The rate limiter is shared with the token-per-minute governor in `governance.ratelimit_max_rpm`.

## Scope

Security vulnerabilities include but are not limited to:

- Authentication/authorization bypasses
- Request smuggling or HTTP desync attacks
- Denial of service (resource exhaustion)
- Information disclosure (leaked secrets, headers, or internal state)
- SSRF or injection vulnerabilities (the defense stack is documented in
  [INJECTION_DEFENSE.md](INJECTION_DEFENSE.md): prompt-injection detection,
  spotlighting, output ExfilGuard, canary tripwires, and MCP tool-call
  argument guards)
- Path traversal attacks on config file loading and prompt file resolution

### File Path Security

Config file loading functions (`LoadConfig`, `LoadSecrets`, `LoadPromptFile`) are hardened against path traversal attacks:

- All user-supplied paths are resolved via `filepath.Abs` and checked against the config directory prefix
- Paths containing `..` segments are rejected
- Absolute paths pointing outside the config root are rejected
- Extensive test coverage in `config/loading_path_security_test.go` validates all traversal vectors

Issues outside scope (feature requests, bugs without security impact) should be reported via [GitHub Issues](https://github.com/gumieri/nenya/issues).
