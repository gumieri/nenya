# MCP Integration (Model Context Protocol)

Nenya integrates with external MCP (Model Context Protocol) servers to give AI agents access to external tools. When configured, agents can discover, inject, and execute MCP tools through a transparent multi-turn loop — all transparently to the client.

## How It Works

```
Client Request (model: "build")
  │
  ├─ Resolve agent → "build" has MCP servers configured
  ├─ MCP Tool Discovery (at startup)
  │   └─ Connect to MCP servers via HTTP+SSE, list all available tools
  ├─ MCP Tool Injection (per-request)
  │   └─ Transform MCP tools into OpenAI function tools, inject into request
  ├─ MCP Auto-Search (optional, per-request)
  │   └─ Query MCP server for relevant context, inject as system message
  ├─ Content Pipeline (redaction, compaction, etc.)
  ├─ Forward to upstream with MCP tools → buffer SSE response
  ├─ Inspect buffered response:
  │   ├─ No tool_calls → replay to client, done
  │   ├─ Client tool_calls only → replay to client, done
  │   ├─ MCP tool_calls → execute via MCP client, append results, re-send
  │   │   └─ Loop (up to max_iterations)
  │   └─ Max iterations → replay last response
  ├─ Stream response to client
  └─ MCP Auto-Save (optional, async)
      └─ Store assistant response to MCP server (best-effort)
```

### MCP Tool Discovery (At Startup)

1. Nenya reads `mcp_servers` from config
2. For each server, creates an MCP client and connects via HTTP+SSE
3. Sends `initialize` + `notifications/initialized` handshake
4. Calls `tools/list` to discover all available tools
5. Builds a `ToolRegistry` mapping `server__tool` → MCP tool details
6. If a server is unreachable, its tools are silently omitted (warning logged)

### Tool Injection (Per-Request)

1. Nenya checks if the resolved agent has an `mcp` configuration
2. For each referenced server, transforms MCP tools into OpenAI function tool format
3. Tools are namespaced as `server__tool_name` (e.g., `mempalace__mempalace_search`)
4. Tools are appended to the request's existing `tools[]` array
5. A system prompt is injected instructing the LLM to use the MCP tools for memory/knowledge retrieval
6. The `prefix_cache.stable_tools` optimization sorts all tools (including MCP) for deterministic ordering

### Multi-Turn Tool Execution (Per-Request)

1. The upstream response is buffered completely in memory (not streamed to client)
2. The buffered SSE is parsed for `tool_calls` in the response
3. The assistant message (with `tool_calls` or content) is reconstructed from the buffered stream
4. Tool calls are partitioned into MCP tools vs client tools:
    - **MCP tools**: Executed locally via the MCP client (parallel)
    - **Client tools**: Passed through to the client unmodified
5. The reconstructed assistant message is appended to the messages array
6. MCP tool results are appended as `tool` role messages after the assistant message
7. The request is re-sent to the upstream
8. This loops until: content response, client-only tools, or max iterations reached

### Streaming Argument Accumulation

MCP tool call arguments are streamed incrementally by the upstream LLM. Nenya accumulates argument fragments across SSE chunks using the tool call's `index` field, then parses the complete JSON when building the final tool call.

## Configuration

### Server Configuration

MCP servers are configured at the top level:

```json
{
  "mcp_servers": {
    "mempalace": {
      "url": "http://localhost:6060",
      "timeout": 30
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | string | (required) | URL of the MCP HTTP+SSE proxy (e.g., `http://host:port`) |
| `timeout` | int | `30` | Per-tool-call timeout in seconds |
| `headers` | object | (none) | Additional HTTP headers sent to the MCP server (e.g., auth) |

### Agent Configuration

MCP integration is enabled per-agent:

```json
{
  "agents": {
    "build": {
      "strategy": "fallback",
      "mcp": {
        "servers": ["mempalace"],
        "max_iterations": 10,
        "auto_search": true,
        "auto_save": true
      },
      "models": ["gemini-2.5-flash", "deepseek-reasoner"]
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `servers` | []string | (none) | Names of MCP servers from the top-level `mcp_servers` config |
| `max_iterations` | int | `10` | Maximum MCP tool call rounds per request |
| `auto_search` | bool | `false` | Automatically search MCP servers for relevant context before forwarding |
| `auto_save` | bool | `false` | Automatically store assistant responses to MCP server after streaming |
| `search_tool` | string | (auto) | MCP tool name for auto-search. Auto-discovers a tool prefixed with `search` if not set. |
| `save_tool` | string | (auto) | MCP tool name for auto-save. Auto-discovers a tool prefixed with `add` or `save` if not set. |

## Running an MCP Server

### MemPalace via `mcp proxy`

```bash
# Clone and install MemPalace
cd /path/to/mempalace
pip install -e .

# Start MCP proxy (default port 6060)
mcp proxy
```

Nenya connects to the proxy via HTTP+SSE. The proxy exposes:
- `GET /sse` — SSE connection for receiving the session endpoint
- `POST /message` — JSON-RPC 2.0 endpoint for tool calls

To change the proxy port:

```bash
mcp proxy --port 6060
```

### Multiple MCP Servers

Agents can reference multiple servers:

```json
{
  "mcp_servers": {
    "mempalace": {
      "url": "http://localhost:6060"
    },
    "files": {
      "url": "http://localhost:8081",
      "headers": {
        "Authorization": "Bearer token"
      }
    }
  }
}
```

```json
{
  "agents": {
    "build": {
      "mcp": {
        "servers": ["mempalace", "files"]
      }
    }
  }
}
```

## Tool Name Namespacing

MCP tools are injected with the pattern `server__tool_name` to avoid collisions across servers:

| MCP Server | MCP Tool | Injected Name |
|-------------|----------|--------------|
| `mempalace` | `mempalace_search` | `mempalace__mempalace_search` |
| `mempalace` | `add_drawer` | `mempalace__add_drawer` |
| `files` | `read_file` | `files__read_file` |

Non-MCP tool calls (from the client like `file_edit`, `bash`) pass through unmodified.

## Graceful Degradation

MCP integration follows the same best-effort philosophy as the rest of Nenya:

- **Server unreachable at startup**: Tools from that server are silently omitted. A warning is logged. The request proceeds normally without those tools.
- **Server goes down mid-session**: Tool calls fail with error results that are returned to the LLM as tool result messages. The LLM can inform the user or try a different approach.
- **Timeout**: Each MCP tool call has a configurable timeout (default 30s per-server). Timeouts return error results.
- **Max iterations**: The multi-turn loop has a configurable max iteration count (default 10). When exhausted, the last buffered response is replayed to the client.
- **No MCP configured**: Zero overhead — no allocation, no goroutine, no tool injection.

## Security Considerations

- MCP servers run on the local network. Ensure they are trusted before connecting.
- The `headers` field allows passing authentication to MCP proxies that require it.
- Tool call arguments are passed through to MCP servers as-is. Nenya does not sanitize MCP tool call arguments.
- MCP server responses (tool results) are injected directly into the LLM conversation as tool messages. The LLM sees them unmodified.
- Timeout values prevent hanging connections to slow MCP servers.

## Memory Migration (mem0 to MCP)

Nenya supports a graceful migration path from mem0 to MCP-based memory:

1. **If both MCP and mem0 are configured for an agent**, MCP takes priority. Nenya checks for MCP servers with a `search`-prefixed tool first.
2. **If no MCP servers are configured**, Nenya falls back to mem0 (if configured).
3. **If neither is configured**, no memory context is injected — zero overhead.

This means existing mem0 deployments continue working unchanged, and MCP can be adopted incrementally.

## Transport Details

Nenya connects to MCP servers via HTTP+SSE (Server-Sent Events). Two transport modes are supported:

### Direct Mode (HTTP 200)

The MCP server returns JSON-RPC responses directly in the POST response body with `HTTP 200`. This is the simpler mode used by some MCP implementations.

### Proxy Mode (HTTP 202 + SSE)

The `mcp-proxy` (from `@modelcontextprotocol/sdk`) uses a different pattern:

1. `GET /sse` — Opens a long-lived SSE connection. Returns an `endpoint` event with the session URL (e.g., `/messages?sessionId=UUID`).
2. `POST /messages?sessionId=...` — Returns `HTTP 202 Accepted` with body `Accepted`. The actual JSON-RPC response arrives via the SSE stream as an `event: message`.
3. The SSE connection must remain open for the session to stay alive. Closing the SSE connection destroys the session.

Nenya handles both modes transparently in the transport layer. If the POST returns 202, it waits for the response on the SSE event dispatch channel. If the POST returns 200, it parses the body directly.
