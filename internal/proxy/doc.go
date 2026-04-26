// Package proxy implements the HTTP request routing, streaming, and forwarding
// logic for the Nenya API gateway. It handles chat completions with content
// pipeline interception (redaction, summarization), SSE stream proxying with
// provider-specific transformations, MCP tool injection and multi-turn loops,
// retry with exponential backoff, and passthrough proxying for arbitrary endpoints.
package proxy
