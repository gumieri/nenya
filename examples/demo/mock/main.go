// Command mock is a minimal OpenAI-compatible mock upstream for the Nenya
// demo harness (examples/demo). It serves a static model catalog, dumps
// every chat completion request body it receives to received.json in its
// working directory (so the demo can show exactly what left the gateway),
// and replies with a tiny SSE stream. It listens on 127.0.0.1:9999.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const addr = "127.0.0.1:9999"

// dumpPath is where received chat completion bodies are written.
// Overridable via DEMO_DUMP_DIR so the harness can point it at an
// absolute path regardless of the server's working directory.
var dumpPath = "received.json"

func main() {
	if dir := os.Getenv("DEMO_DUMP_DIR"); dir != "" {
		dumpPath = filepath.Join(dir, "received.json")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", handleModels)
	mux.HandleFunc("POST /v1/chat/completions", handleChat)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	log.Printf("mock upstream listening on %s (dump: %s)", addr, dumpPath)
	log.Fatal(srv.ListenAndServe())
}

func handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"demo-model","object":"model","owned_by":"demo"}]}`)
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(dumpPath, body, 0o600); err != nil {
		// The demo needs this dump, but failing the request outright would
		// make the harness unusable if the workdir disappeared; degrade and
		// keep serving so the failure is visible in the dump file only.
		log.Printf("WARN: could not dump request body: %v", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-demo\",\"object\":\"chat.completion.chunk\",\"model\":\"demo-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	flusher.Flush()
	_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-demo\",\"object\":\"chat.completion.chunk\",\"model\":\"demo-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Config looks good — and note: your secrets never reached this upstream.\"},\"finish_reason\":null}]}\n\n")
	flusher.Flush()
	_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-demo\",\"object\":\"chat.completion.chunk\",\"model\":\"demo-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	flusher.Flush()
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
