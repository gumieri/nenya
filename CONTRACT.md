# Nenya Consumer Contract

**Contract version: `1`** · Status: **spec-first** (normative target)

This document is the interface contract between Nenya and any external manager or
tooling that installs, configures, or observes it — primarily
[nenyactl](https://github.com/gumieri/nenyactl).

The rule is one-directional:

> **Nenya owns the mechanism. The consumer owns the invocation.**

An external tool must be able to operate correctly **without reading Nenya's
source code**. Every fact it needs is in this document, exposed through a
versioned CLI surface and machine-readable JSON.

The contract is implemented with the Go standard library only; it does not
weaken Nenya's zero-dependency guarantee.

## How to read the status column

Every surface is tagged:

| Tag | Meaning |
|-----|---------|
| **stable** | Implemented and guaranteed today. |
| **target** | Normative in contract v1, to be implemented. Consumers may code against it; until it ships, feature-detect. |
| **internal** | Explicitly **not** contract. May change at any time. |

---

## Table of contents

1. [Scope and stability](#1-scope-and-stability)
2. [Contract versioning](#2-contract-versioning)
3. [CLI surface (stable)](#3-cli-surface-stable)
4. [CLI surface (target)](#4-cli-surface-target)
5. [On-disk layout and precedence](#5-on-disk-layout-and-precedence)
6. [Secrets](#6-secrets)
7. [Release artifacts](#7-release-artifacts)
8. [Service units and lifecycle](#8-service-units-and-lifecycle)
9. [HTTP surface for consumers](#9-http-surface-for-consumers)
10. [Change process](#10-change-process)
11. [Appendix A — JSON shapes](#appendix-a--json-shapes)
12. [Appendix B — status matrix](#appendix-b--status-matrix)

---

## 1. Scope and stability

### 1.1 What is contract

- The CLI flags, subcommands, and JSON outputs listed in this document.
- Environment variables listed in this document.
- On-disk config and secrets layout **and precedence** (§5, §6).
- Release artifact names and archive member names (§7).
- Service unit semantics and reload behavior (§8).
- The HTTP endpoints and auth rules listed in §9.
- Signal handling and process exit codes (§3.4 and §8.3).

### 1.2 What is not contract (`internal`)

Consumers MUST NOT depend on any of the following; they may change in any
release without a contract bump:

- Go package paths, exported types, functions, or structs.
- The `github.com/nenya/...` module and its internal packages.
- Config field **ordering**, log message text, and log field names.
- Prometheus metric names/labels beyond those explicitly listed in `README.md`.
- `/statsz` payload shape (it is an operator view, not a machine API).
- The exact contents of the shipped config example.

---

## 2. Contract versioning

- The contract is identified by an integer `contract_version`, currently **`1`**.
- It is surfaced by `nenya version --json` and `nenya describe --json`
  (`contract_version` field), and **target** as `nenya --contract-version`.
- Additive changes (new fields, new commands) keep the same version.
- Removing or renaming a field, or changing its meaning/type, **BUMPS** the
  version.
- Consumers MUST declare the range they support and fail fast with an
  actionable message when the installed Nenya is out of range.
  Recommended consumer constant: `SupportedContract = [1, 1]`.

---

## 3. CLI surface (stable)

### 3.1 Invocation

```
nenya [flags]
```

Nenya is a long-running server. It has no subcommands in the current release;
the target surface in §4 adds inspection commands that exit immediately.

### 3.2 Flags

| Flag | Type | Default | Contract meaning |
|------|------|---------|------------------|
| `-config <path>` | string | — | Single-file config mode. `<path>` MUST be a file, not a directory. |
| `-config-dir <path>` | string | `/etc/nenya/` (effective) | Directory config mode (see §5). |
| `-verbose` | bool | `false` | Debug-level logging. |
| `-validate` | bool | `false` | Validate config + secrets, then exit. |
| `-print-config-schema` | bool | `false` | Print the config JSON Schema to stdout, then exit. |

The literal flag default for `-config-dir` is empty; `/etc/nenya/` is the
effective fallback when neither a directory nor a file is selected.

**Mode selection:** file mode if `-config`/`NENYA_CONFIG_FILE` is set;
otherwise directory mode. `-config` pointing at a directory is an error.

**Target:** unknown flags MUST fail closed (non-zero exit + usage on stderr).
Today unknown flags are silently ignored because the parse error is discarded,
which lets a typo start the server against the default config.

### 3.3 Environment variables

| Variable | Effect | Precedence |
|----------|--------|------------|
| `NENYA_CONFIG_FILE` | Single config file path | Overrides `-config`; forces file mode |
| `NENYA_CONFIG_DIR` | Config root directory | Overrides `-config-dir` |
| `NENYA_SECRETS_DIR` | Secrets directory | Overrides the default `/run/secrets/nenya`, but **not** `CREDENTIALS_DIRECTORY` |
| `CREDENTIALS_DIRECTORY` | systemd credential dir | Highest-priority secrets source |
| `CONFIG_DIR` | Config root used for prompt-file path validation | — |
| `PORT` | Listening port | Replaces `server.listen_addr` with `:<PORT>`; a host in `server.listen_addr` is discarded unless `HOST` is also set |
| `HOST` | Bind address | Only applied together with `PORT` |

`NENYA_CONFIG_FILE` wins over `NENYA_CONFIG_DIR` when both are set.

### 3.4 Exit codes

| Code | Meaning |
|------|---------|
| `0` | Clean exit: validation passed, or the server shut down gracefully. |
| `1` | Any failure: config/secrets load error, validation failure, listener error, server error, or a timed-out drain/shutdown. |

Machine-readable output (JSON from `-print-config-schema`, and all §4 commands)
is written to **stdout**. Diagnostics and errors are written to **stderr**.

---

## 4. CLI surface (target)

These commands are normative in contract v1. Consumers MUST feature-detect
(e.g. by attempting the command and checking exit status) until they ship.

### 4.1 `nenya version --json` — status: target

Exits `0` and prints a single JSON object (Appendix A.1). Also accepts the
conventional `--version` flag with the same output. **This replaces the current
absence of any CLI version surface**, which is a known defect: `nenya --version`
today falls through to server startup because unknown flags are ignored.

### 4.2 `nenya paths --json` — status: target

Prints the resolved filesystem contract (Appendix A.2): config dir/file,
`config.d`, secrets dir, and socket path. Resolution MUST match what the server
actually uses for the same flags/environment.

### 4.3 `nenya describe --json` — status: target

Prints the effective state a manager needs to render UI and take decisions
(Appendix A.3): resolved paths, effective merged config, secrets sources
searched and the one that won, provider/model catalog, diagnostics, and
`contract_version`.

`describe` is the **single source of truth** for "what config is actually in
effect". A consumer MUST NOT reproduce the merge logic itself.

### 4.4 `nenya example-config` — status: target

Prints the canonical example config (JSONC) to stdout. Consumers MUST use this
instead of embedding a copy. The example MUST load successfully under the
current schema.

### 4.5 `nenya service-unit` — status: target

Prints a service unit to stdout so consumers never parse unit files out of a
release archive.

| Flag | Default | Meaning |
|------|---------|---------|
| `--init` | `systemd` | `systemd` or `launchd` |
| `--exec-path` | `/usr/bin/nenya` | Absolute path to the installed binary |
| `--config-dir` | `/etc/nenya` | Config root the unit should pass to the binary |
| `--secrets-file` | `/etc/nenya/secrets.json` | Credential source wired into the unit |

The emitted unit MUST be valid for the requested init system and MUST reference
the supplied paths.

### 4.6 `nenya config set <dotted.key> <value>` — status: target

Sets a config value using Nenya's own precedence, writing to the canonical
location (§5). `<value>` is parsed as JSON when valid, otherwise as a string.
Writes are atomic; the command prints the path it modified. This is a **single
writer**: consumers MUST call it rather than editing config files directly.

Examples:
```
nenya config set server.listen_addr '":9090"'
nenya config set discovery.auto_agents true
```

### 4.7 `nenya secret set …` — status: target

The secrets single writer.

| Form | Effect |
|------|--------|
| `nenya secret set --provider <name> <api-key>` | Sets `provider_keys[name]` |
| `nenya secret set --client-token [<token>]` | Sets (or generates) `client_token` |

Files are created with mode `0600`; existing unrelated keys are preserved.

---

## 5. On-disk layout and precedence

### 5.1 Directory mode (default)

```
<config-root>/                 (default /etc/nenya/)
├── config.json                # single-file form
└── config.d/                  # multi-file form
    ├── 00-server.json
    ├── 20-agents.json
    └── …
```

**Precedence — CURRENT (`stable`):** if `<config-root>/config.d/` exists and
contains **at least one** `*.json` file (excluding `secrets.json`), those files
are merged in ascending name order and **`config.json` is NOT read at all**.
Otherwise `config.json` is read.

> ⚠️ This mutual exclusivity is a known footgun: writing a single drop-in
> silently discards a populated `config.json`. The `config.d`/`config.json`
> XOR is scheduled to be removed under the *Config & Secrets Correctness*
> workstream. **When it changes, `contract_version` semantics for §5 are
> re-stated here in the same release.**

Merge semantics across `config.d/*.json`:

- Map fields (`agents`, `providers`, `mcp_servers`) merge per key.
- Every other field is **field-level last-wins**: for plain scalar fields a
  zero/empty value does not clear an earlier value (last-non-zero), while
  fields backed by an explicit `*WasSet` marker (e.g.
  `server.secure_memory_required`, the governance bools,
  `bouncer.enabled`/`bouncer.fail_open`) treat an explicit `false`/`0` as
  last-file-wins.
- `secrets.json` inside `config.d/` is ignored.

> ⚠️ **Known gap (tracked).** The directory merge does not yet cover every
> nested field: some `governance.*` sub-sections (e.g. `injection`, `spotlight`,
> `exfil_guard`, `canary`, `param_compat`) set only in `config.d/` are silently
> dropped. Until this is fixed, governance security settings SHOULD live in a
> single file — either `-config <file>` (file mode) or a config root that
> contains only `config.json` and no `config.d/`. Consumers must treat
> `nenya describe` as the authority on what is actually in effect.

### 5.2 File mode

`-config <file>` / `NENYA_CONFIG_FILE` loads exactly one file. `config.d` is not
consulted.

### 5.3 Format

- JSON with `//` and `/* */` comments stripped before decoding.
- Unknown fields are **warned about**, never fatal.
- The authoritative schema is `nenya -print-config-schema` (JSON Schema).

### 5.4 File modes (conventional)

| Path | Mode |
|------|------|
| config directory | `0755` |
| config files | `0644` |
| secrets files | `0600` |

These are install-time conventions; **the loader does not set or enforce them**.
Only the secrets modes are security-relevant, and they are the responsibility of
the installing tool (and `nenya secret set`, §4.7).

---

## 6. Secrets

### 6.1 Sources (first match wins)

| Priority | Source |
|----------|--------|
| 1 | `$CREDENTIALS_DIRECTORY/secrets` (single file) |
| 2 | `$CREDENTIALS_DIRECTORY/secrets.d/*.json` (merged by name) |
| 3 | `$NENYA_SECRETS_DIR/*.json` (merged by name) |
| 4 | `/run/secrets/nenya/*.json` (merged by name) |

A missing candidate falls through to the next source. A read error on the
single `$CREDENTIALS_DIRECTORY/secrets` file is also treated as absent; a
stat/read failure on a directory source (including a permission error) is
**fatal**. Once a source is selected, a parse or validation failure is fatal —
Nenya does not fall through to the next source.

### 6.2 Shape

```jsonc
{
  "client_token": "nk-…",              // required, used for /v1/* bearer auth
  "provider_keys": { "gemini": "AIza…" },
  "api_keys": { /* per-key RBAC, see docs/SECRETS_FORMAT.md */ }
}
```

`client_token` is required (`contract_version` 1). Multiple files merge
per top-level key: `provider_keys` merges per provider, the last non-empty
`client_token` wins, and `api_keys` entries are replaced per key — a later entry
overrides an earlier one only when it is `enabled: true`.

### 6.3 Not contract

Whether a given deployment stores secrets on disk, via systemd credentials, or
via a container secret mount — only the **source priority order** above is
contract.

---

## 7. Release artifacts

### 7.1 Archives

| Artifact | Platform |
|----------|----------|
| `nenya_<version>_linux_{amd64,arm64}.tar.gz` | Linux |
| `nenya_<version>_darwin_{amd64,arm64}.tar.gz` | macOS |

Archive members, as stored in the tarball:

| Member | Present on |
|--------|-----------|
| `nenya` | all |
| `deploy/nenya.service`, `deploy/nenya.socket` | linux |
| `deploy/nenya.plist` | darwin |

Consumers **MUST** extract the `nenya` member by exact name. Unit files are
present under `deploy/`, but consumers **MUST NOT** depend on parsing them out
of the archive (their location may change); use `nenya service-unit` (§4.5).
No config example is shipped in the archives — use `nenya example-config`
(§4.4).

### 7.2 Integrity

| Artifact | Meaning |
|----------|---------|
| `checksums.txt` | SHA-256 of every release artifact |
| `checksums.txt.sigstore.json` | cosign `sign-blob` bundle for `checksums.txt` |
| `*.spdx.json` | SPDX SBOMs (release + per-container-arch) |

A consumer that downloads and installs a Nenya binary **MUST**:
1. download `checksums.txt` and its sigstore bundle,
2. verify the bundle against the expected release identity, and
3. verify the archive's SHA-256 against `checksums.txt`
   **before** extracting or installing.

Installing an unverified binary is a contract violation.

### 7.3 Packages and images

| Channel | Identifier |
|---------|------------|
| Debian/Ubuntu | `nenya_<version>_linux_{amd64,arm64}.deb` |
| Fedora/RHEL | `nenya_<version>_linux_{amd64,arm64}.rpm` |
| Arch | `nenya_<version>_linux_{amd64,arm64}.pkg.tar.zst`, AUR `nenya-bin` |
| Nix | `gumieri/nur-packages` → `nenya` |
| Container | `ghcr.io/gumieri/nenya:<version>` and `:latest` |

Package install paths (`/usr/bin/nenya`, unit drop-in locations) are documented
by each packaging script and are **not** part of this filesystem contract.

---

## 8. Service units and lifecycle

### 8.1 Unit names

| Init system | Unit |
|-------------|------|
| systemd | `nenya.service` (socket unit: `nenya.socket`) |
| launchd | file `nenya.plist`, `Label` `com.gumieri.nenya` |

### 8.2 Credential wiring

The shipped systemd unit loads secrets via:
```ini
LoadCredential=secrets:/etc/nenya/secrets.json
```
Consumers using a different config root **MUST** regenerate the unit with
`nenya service-unit --secrets-file …` rather than editing the shipped file
blindly.

### 8.3 Signals

| Signal | Behavior |
|--------|----------|
| `SIGHUP` | Reload config + secrets, re-discover models. On validation failure: log and **keep serving the old config**. If the interceptor chain cannot be rebuilt, the process exits `1` (fail-closed). |
| `SIGTERM`, `SIGINT` | Graceful shutdown: HTTP drain (up to 30s, new requests get `503`), then bounded teardown. Exit `0` on clean drain, `1` on timeout. |

Reload MUST be the supported way to apply config changes; consumers SHOULD NOT
restart for config edits.

---

## 9. HTTP surface for consumers

Default listen address `:8080` (override via `server.listen_addr`, `PORT`, or
`HOST`+`PORT`).

### 9.1 Unauthenticated

| Path | Purpose |
|------|---------|
| `GET /healthz` | Health probe. **Consumers MUST poll `/healthz`** (not `/health`), no auth. |
| `GET /statsz` | Operator view (not contract shape). |
| `GET /metrics` | Prometheus exposition. |

### 9.2 Authenticated

All `/v1/*` and `/proxy/*` routes require
`Authorization: Bearer <client_token|api_key_token>`. API keys additionally
enforce RBAC (roles, agent scoping, endpoint allowlists). See `README.md` and
`docs/SECRETS_FORMAT.md`.

Primary consumer endpoint: `GET /v1/models` (model catalog) and
`POST /v1/chat/completions` (SSE).

`GET /debug/pprof/*` requires bearer auth and is disabled unless
`debug.pprof_enabled` is set.

---

## 10. Change process

1. Any change to §3–§9 is reviewed against this document.
2. Additive changes leave `contract_version` unchanged and are noted in
   `CHANGELOG.md`.
3. Breaking changes bump `contract_version` in the same release.
4. Deprecations are announced for at least one minor release before removal.
5. Consumers that discover an unsupported `contract_version` MUST fail with a
   message naming the installed and supported versions.

---

## Appendix A — JSON shapes

Field names below are normative once the command is `stable`.

### A.1 `nenya version --json`

```json
{
  "version": "0.15.0",
  "commit": "a80e0cf8ce05176fc1159d03d0fc5685c934ebaa",
  "build_time": "2026-09-26T03:08:44Z",
  "contract_version": 1
}
```

### A.2 `nenya paths --json`

```json
{
  "mode": "directory",
  "config_dir": "/etc/nenya",
  "config_file": "/etc/nenya/config.json",
  "config_d": "/etc/nenya/config.d",
  "secrets_dir": "/run/secrets/nenya",
  "socket_path": null,
  "platform": "linux"
}
```

`mode` is `"file"` or `"directory"`. Paths are absolute. `socket_path` is
`null` unless a Unix-domain socket is configured — the shipped socket unit uses
TCP (`ListenStream=8080`), in which case the process consumes `LISTEN_FDS`
rather than a path.

### A.3 `nenya describe --json`

```json
{
  "contract_version": 1,
  "version": { "version": "…", "commit": "…", "build_time": "…" },
  "paths": { /* same as A.2 */ },
  "secrets": {
    "active_source": "/run/secrets/nenya",
    "searched": [
      "<CREDENTIALS_DIRECTORY>/secrets",
      "<CREDENTIALS_DIRECTORY>/secrets.d",
      "<NENYA_SECRETS_DIR or /run/secrets/nenya>"
    ]
  },
  "config": { /* effective merged config, per -print-config-schema */ },
  "providers": {
    "configured": ["gemini", "deepseek"],
    "catalog": [ /* {provider, model, context_window, max_output} */ ]
  },
  "diagnostics": [
    { "level": "warn", "code": "unknown_field", "message": "…", "source": "config.d/20-agents.json" }
  ]
}
```

The `searched` list mirrors §6.1 in priority order; `CREDENTIALS_DIRECTORY` is
platform-provided. `config` is the effective configuration after merge and
defaults, and is the authoritative answer to "what is in effect". Its shape is
defined by the schema from `nenya -print-config-schema`; only
`contract_version`, `paths`, `secrets`, and `diagnostics` are individually
guaranteed here.

---

## Appendix B — status matrix

| Surface | Status |
|---------|--------|
| `-config`, `-config-dir`, `-verbose`, `-validate`, `-print-config-schema` | stable |
| Environment variables (§3.3) | stable |
| Directory/file mode precedence (§5.1) | stable (XOR being revised) |
| Secrets source order (§6.1) | stable |
| Release archive members (§7.1) | stable |
| Checksums + cosign bundle (§7.2) | stable |
| Signals + reload semantics (§8.3) | stable |
| `/healthz`, auth rules (§9) | stable |
| `version --json` / `--version` (§4.1) | **target** |
| `paths --json` (§4.2) | **target** |
| `describe --json` (§4.3) | **target** |
| `example-config` (§4.4) | **target** |
| `service-unit` (§4.5) | **target** |
| `config set` (§4.6) | **target** |
| `secret set` (§4.7) | **target** |
| `--contract-version` (§2) | **target** |
