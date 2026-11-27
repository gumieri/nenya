package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nenya/internal/pseudonym"
)

// rate is num/den with the zero-denominator guard.
func rate(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// randomSalt generates the default salt epoch (128 bits) so an unattended
// run never reuses a stale salt. The report prints it for reproducibility.
func randomSalt() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Crypto/rand failure at spike-harness startup is unrecoverable.
		panic("pseudonym-spike: salt generation failed: " + err.Error())
	}
	return []byte(hex.EncodeToString(b))
}

// liveScenario is the synthetic multi-turn conversation the LIVE half of
// the measurement dispatches: the same declared terms recur across turns,
// so a deterministic pseudonym keeps the patched prefix byte-stable — the
// property the cache-hit comparison measures.
var liveScenario = []string{
	"Hi, I'm Maria Silva from Acme Corp. Our billing portal is acme.com and the API key sk-live-9f4a7b2c stopped working after the rotate.",
	"The sk-live-9f4a7b2c key now works, but Acme Corp's invoice for acme.com still shows the old plan. Can you check?",
	"Thanks. Please draft an email to Acme Corp billing asking them to fix the acme.com invoice, signed by Maria Silva.",
	"Last thing: summarize what we changed about the sk-live-9f4a7b2c key and the acme.com plan so I can file it.",
}

// emitRequests writes the operator live-run request bodies: the same
// cumulative 4-turn conversation in unpatched and patched spellings, one
// JSON file per turn, OpenAI chat-completions format (the runbook's
// provider appendix maps Anthropic's /v1/messages + cache_control delta).
// The scenario is SYNTHETIC — real declared terms never enter emitted
// files.
func emitRequests(dir string, table *pseudonym.Table, salt []byte) error {
	if err := os.MkdirAll(filepath.Join(dir, "unpatched"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "patched"), 0o700); err != nil {
		return err
	}
	p := pseudonym.NewPatcher(table, salt)
	patchedTurns := make([]string, len(liveScenario))
	for i, turn := range liveScenario {
		patchedTurns[i] = p.Patch(turn).Text
	}
	for i := range liveScenario {
		history := make([]map[string]string, 0, i+1)
		for j := 0; j <= i; j++ {
			// The scenario is a user-driven fix thread: every turn is a
			// user message, cumulative (the cache prefix grows per turn).
			history = append(history, map[string]string{"role": "user", "content": liveScenario[j]})
		}
		if err := writeTurn(filepath.Join(dir, "unpatched", fmt.Sprintf("turn%d.json", i+1)), history); err != nil {
			return err
		}
		patchedHistory := make([]map[string]string, 0, i+1)
		for j := 0; j <= i; j++ {
			patchedHistory = append(patchedHistory, map[string]string{"role": "user", "content": patchedTurns[j]})
		}
		if err := writeTurn(filepath.Join(dir, "patched", fmt.Sprintf("turn%d.json", i+1)), patchedHistory); err != nil {
			return err
		}
	}
	fmt.Printf("pseudonym-spike: live-run request bodies emitted under %s (unpatched/ + patched/); see docs/SPIKE_REVERSIBLE_PSEUDONYM.md for the operator protocol\n", dir)
	return nil
}

// writeTurn marshals one cumulative conversation body.
func writeTurn(path string, history []map[string]string) error {
	body := map[string]interface{}{
		"model":    "OPERATOR-MODEL",
		"messages": history,
		"stream":   false,
	}
	encoded, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}
