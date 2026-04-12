# Memory Integration (mem0)

Nenya integrates with [mem0](https://github.com/mem0ai/mem0) to provide long-term memory for AI agents. When configured, agents automatically search for relevant past memories before each request and store conversation history after each response — all transparently to the client.

## How It Works

```
Client Request (model: "build")
  │
  ├─ Resolve agent → "build" has memory configured
  ├─ Memory Search (synchronous, 5s timeout)
  │   └─ POST /search to mem0 OSS server with last user message
  ├─ Inject Memories
  │   └─ Insert system message with relevant memories before last user message
  ├─ Content Pipeline (redaction, compaction, etc.)
  ├─ Forward to upstream (streaming)
  ├─ Capture assistant response via OnContent callback
  └─ Memory Store (async, 10s timeout)
      └─ POST /memories to mem0 OSS server with assistant content
```

### Memory Search (Before Upstream)

1. Nenya checks if the resolved agent has a `memory` configuration
2. The last user message content is extracted as the search query
3. A `POST /search` request is sent to the mem0 server with `user_id`, `top_k`, and `threshold`
4. Results are formatted as a bullet list and injected as a dedicated system message before the last user message
5. This placement improves AI recall (context adjacent to query) and maintains system prompt stability for prefix caching

### Memory Storage (After Stream)

1. The `SSETransformingReader` captures delta content chunks via the `OnContent` callback
2. After the stream completes successfully, the accumulated assistant response is stored asynchronously
3. A `POST /memories` request is sent with the assistant content and `user_id`
4. This runs in a background goroutine — failures are logged but never block the response

### Best-Effort Design

Memory operations follow the same best-effort philosophy as the rest of Nenya's pipeline:

- **Search failures**: Logged as warning, request proceeds without memories
- **Storage failures**: Logged as warning, response already delivered to client
- **No memory configured**: Zero overhead — no allocation, no goroutine, no network call

## Configuration

Memory is configured per-agent in the `agents` section:

```json
{
  "agents": {
    "build": {
      "strategy": "fallback",
      "memory": {
        "url": "http://127.0.0.1:8081",
        "user_id": "default",
        "top_k": 10,
        "threshold": 0.3
      },
      "models": ["gemini-2.5-flash", "deepseek-reasoner"]
    }
  }
}
```

### Memory Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | string | (required) | Base URL of the mem0 OSS server (e.g., `http://127.0.0.1:8081`) |
| `user_id` | string | (required) | User identifier for memory isolation. All memories are scoped to this user. |
| `top_k` | int | `10` | Maximum number of memories to retrieve per search |
| `threshold` | float | `0.3` | Minimum relevance score (0.0-1.0) for returned memories |

### API Key

The mem0 API key is configured in the secrets file:

```json
{
  "client_token": "...",
  "memory_provider_keys": {
    "mem0": "your-mem0-api-key"
  },
  "provider_keys": { ... }
}
```

If `memory_provider_keys["mem0"]` is set in secrets and the agent's `memory` config does not include an `api_key` field, the secret key is automatically injected. If the agent config has an explicit `api_key`, it takes priority.

Authentication uses the `X-API-Key` header (mem0 OSS convention). If no key is configured, the header is omitted.

## Running mem0

### Docker (recommended)

```bash
docker run -d \
  --name mem0 \
  -p 8081:8081 \
  -e MEM0_API_KEY=your-api-key \
  mem0ai/mem0:latest
```

### Without Authentication (local development)

```bash
docker run -d \
  --name mem0 \
  -p 8081:8081 \
  mem0ai/mem0:latest
```

When no `MEM0_API_KEY` is set, mem0 accepts unauthenticated requests. Omit `mem0` from `memory_provider_keys` in this case.

## Memory Context Format

Memories are injected as a system message with this format:

```
[Relevant memory context]
- User prefers Go for backend services
- Project uses PostgreSQL with Prisma ORM
- Active development on nenya AI gateway
```

This is placed immediately before the last user message in the `messages` array, maximizing the AI's ability to recall relevant context while preserving the system prompt's position for prefix cache optimization.

## Timeouts

| Operation | Timeout | Rationale |
|-----------|---------|-----------|
| Search | 5s | Synchronous — adds to request latency. Must be fast. |
| Store | 10s | Asynchronous — does not block the response. More lenient. |

## Multiple Agents

Each agent can have independent memory configuration with different `user_id` values:

```json
{
  "agents": {
    "build": {
      "memory": { "url": "http://127.0.0.1:8081", "user_id": "dev-user" },
      "models": ["gemini-2.5-flash"]
    },
    "plan": {
      "memory": { "url": "http://127.0.0.1:8081", "user_id": "planner-user" },
      "models": ["deepseek-reasoner"]
    }
  }
}
```

This allows different agents to maintain separate memory spaces for the same or different users.
