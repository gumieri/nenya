// Package mcp implements a client for the Model Context Protocol (MCP) over
// HTTP+SSE transport. It handles JSON-RPC 2.0 message framing, tool discovery,
// tool invocation, and reconnection with backoff. MCP servers provide external
// tools (e.g. long-term memory search) that are injected into upstream AI requests.
package mcp
