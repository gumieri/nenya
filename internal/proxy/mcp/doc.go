// Package mcpproxy extracts Nenya's MCP subsystem out of the chat proxy:
// tool injection, the multi-turn tool loop, the buffered dispatch core,
// the tool-result vision gate, and usage recording.
//
// The subsystem is hosted by the parent proxy package through the Host
// interface: every function here that used to be a *Proxy method takes a
// Host as its first parameter, and the parent supplies proxyHost, a thin
// delegating adapter (built in the parent's mcp_compat.go). Zero behavior
// change is the contract of this extraction (NENYA-312, Phase 040).
//
// Invariants this package must preserve:
//
//   - NENYA-162 injected-tools bookkeeping: the loop's non-injected-call
//     guard is anchored on the gateway's own injection record (forwarded
//     in ForwardOptions.InjectedMCPTools), never on the client-writable
//     payload. Admission resets the client's binding; the set threading
//     through the loop here is the trust anchor.
//   - NENYA-177 hot-path allocation discipline: read-only stages never
//     clone the request. Interceptors mutate message maps in place and
//     the buffered replay path reuses pooled buffers; no stage in this
//     package may introduce a request-wide copy.
//
// Buffered core: BufferStreamResponse accumulates one upstream SSE
// response into BufferedSSE (raw frames, extracted tool calls, usage
// merges), ForwardBuffered sweeps the target chain through the host's
// dispatch (PrepareAndSend) and replays the surviving buffer; the egress
// guard and the canary tripwire run over the buffered frames before any
// replay. UsageRecording claims each dispatch's Stats/metrics/cost/billing
// exactly once (NENYA-193) and feeds the NENYA-135 calibration loop.
package mcpproxy
