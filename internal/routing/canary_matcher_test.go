package routing

import (
	"strings"
	"testing"

	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/pipeline"
)

// TestCanaryMatcherEnvelopeAgreement pins AUD-47 (NENYA-254): the adapter's
// trailing-marker matcher and routing's history census must use the SAME
// matcher semantics — the canonical wire envelope ("[internal-ref] " +
// marker). Pre-fix routing matched the BARE CanaryPrefix anywhere in the
// content, so a message that merely quoted or echoed a canary token (no
// envelope) was excluded from the stable-history census while the adapter
// correctly treated it as real history — the two surfaces disagreed, and a
// multi-turn conversation could be censused as one-shot.
func TestCanaryMatcherEnvelopeAgreement(t *testing.T) {
	bare := "let me repeat the token: " + pipeline.CanaryPrefix + "0123456789abcdef0123456789abcdef"
	enveloped := pipeline.CanaryMarker("0123456789abcdef0123456789abcdef")

	// Routing census: the bare token is REAL history; only the enveloped
	// trailer is gateway-produced. Two messages, one quoting the token:
	// the census must see 2 stable messages and anchor (pre-fix the loose
	// matcher excluded the quoted-token message, censused 1, and skipped
	// as one-shot).
	deps, cfg := autoCacheDeps(t, true)
	payload := map[string]interface{}{
		"model": "claude-test",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first"},
			map[string]interface{}{"role": "assistant", "content": bare},
		},
	}
	opts := adapter.CacheOpts{}
	applyAutoCacheBreakpoints(deps, payload, cfg.PrefixCache, &opts, false)
	if !opts.System || !opts.Messages {
		t.Fatalf("a quoted canary token (no envelope) is real history: the census must see 3 stable messages (opts = %+v)", opts)
	}

	// The enveloped trailer still stands down (existing invariant).
	deps2, cfg2 := autoCacheDeps(t, true)
	payload2 := map[string]interface{}{
		"model": "claude-test",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "only turn"},
			map[string]interface{}{"role": "system", "content": enveloped},
		},
	}
	opts2 := adapter.CacheOpts{}
	applyAutoCacheBreakpoints(deps2, payload2, cfg2.PrefixCache, &opts2, false)
	if opts2.System || opts2.Messages {
		t.Fatalf("the enveloped canary trailer is not history: %+v", opts2)
	}
	var sb strings.Builder
	deps2.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `outcome="skipped_one_shot"`) {
		t.Fatalf("one-shot skip must be recorded:\n%s", sb.String())
	}
}
