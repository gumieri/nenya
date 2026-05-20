// Package local manages the lifecycle of local Ollama models for the Nenya
// gateway. It provides model loading/unloading into GPU memory, session
// tracking with LRU eviction, startup preloading, and integration with the
// routing layer via the LocalEngineCheck interface.
//
// # Lock Ordering
//
// The EngineManager uses two mutexes with a strict ordering to prevent
// deadlocks: em.mu (EngineManager) must be acquired BEFORE sm.mu
// (SessionManager). This ordering is maintained throughout all methods.
//
// # Architecture
//
// Local models are NOT routed through a separate code path. They flow
// through the existing retryLoop → prepareAndSend → streamResponse pipeline
// as standard UpstreamTargets. The SessionManager only handles load/unload
// lifecycle; chat completions are proxied by the existing Ollama provider
// adapter and OllamaTransformer.
package local
