package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// cacheMarkerCount counts every cache_control marker anywhere in a
// converted body.
func cacheMarkerCount(t *testing.T, v interface{}) int {
	t.Helper()
	switch node := v.(type) {
	case map[string]interface{}:
		n := 0
		for k, child := range node {
			if k == "cache_control" {
				n++
				continue
			}
			n += cacheMarkerCount(t, child)
		}
		return n
	case []interface{}:
		n := 0
		for _, child := range node {
			n += cacheMarkerCount(t, child)
		}
		return n
	}
	return 0
}

// cacheEconOpts enables every breakpoint class.
func cacheEconOpts() CacheOpts {
	return CacheOpts{
		System:   true,
		Tools:    true,
		Messages: true,
	}
}

func multiTurnOpenAI(messages []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"model": "gpt-4",
		"messages": append([]interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
		}, messages...),
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "lookup",
					"description": "look something up",
					"parameters":  map[string]interface{}{"type": "object"},
				},
			},
		},
	}
}

// TestCacheBreakpointBudgetAndPlacement pins NENYA-171: with every
// breakpoint class enabled the conversion emits at most Anthropic's four
// markers, each on a BLOCK (system block, last tool block, last content
// block of the second-to-last message), and leaves the FINAL message
// unmarked — the slot reserved for the next turn's continuation.
func TestCacheBreakpointBudgetAndPlacement(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := multiTurnOpenAI([]interface{}{
		map[string]interface{}{"role": "user", "content": "turn one"},
		map[string]interface{}{"role": "assistant", "content": "answer one"},
		map[string]interface{}{"role": "user", "content": "turn two"},
	})

	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-3", true, cacheEconOpts())

	// The fixture enables all three gateway classes: exactly three markers
	// (four would mean the reserved continuation slot was consumed).
	if n := cacheMarkerCount(t, result); n != 3 {
		t.Fatalf("expected exactly three gateway breakpoints, got %d", n)
	}

	system, ok := result["system"].([]interface{})
	if !ok || len(system) != 1 {
		t.Fatalf("system must be a block array with a breakpoint: %#v", result["system"])
	}
	if _, marked := system[0].(map[string]interface{})["cache_control"]; !marked {
		t.Fatal("system block must carry the cache breakpoint")
	}

	tools, ok := result["tools"].([]interface{})
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing: %#v", result["tools"])
	}
	if _, marked := tools[len(tools)-1].(map[string]interface{})["cache_control"]; !marked {
		t.Fatal("the last tool block must carry the cache breakpoint")
	}

	msgs := result["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	if _, marked := last["cache_control"]; marked {
		t.Fatal("the final message must stay unmarked (continuation reservation)")
	}
	if n := cacheMarkerCount(t, last); n != 0 {
		t.Fatalf("the final message must carry no breakpoint at all: %d", n)
	}

	secondLast := msgs[len(msgs)-2].(map[string]interface{})
	content := secondLast["content"].([]interface{})
	lastBlock := content[len(content)-1].(map[string]interface{})
	if _, marked := lastBlock["cache_control"]; !marked {
		t.Fatalf("the second-to-last message's last block must carry the breakpoint: %#v", lastBlock)
	}
}

// TestCacheConversionDeterministic pins the no-self-inflicted-miss
// property: the same request converts to byte-identical bytes on every
// run, so nothing wall-clock-varying is injected into the cacheable
// prefix (NENYA-171).
func TestCacheConversionDeterministic(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := multiTurnOpenAI([]interface{}{
		map[string]interface{}{"role": "user", "content": "turn one"},
		map[string]interface{}{"role": "assistant", "content": "answer one"},
		map[string]interface{}{"role": "user", "content": "turn two"},
	})

	first, err := json.Marshal(a.ConvertOpenAIToAnthropicBody(openai, "claude-3", true, cacheEconOpts()))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(a.ConvertOpenAIToAnthropicBody(openai, "claude-3", true, cacheEconOpts()))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("the conversion is not deterministic:\n%s\n%s", first, second)
	}
}

// canaryText flattens a converted message's content to its text, across
// the string and block-array shapes.
func canaryText(msg map[string]interface{}) string {
	switch c := msg["content"].(type) {
	case string:
		return c
	case []interface{}:
		var b strings.Builder
		for _, partRaw := range c {
			if part, ok := partRaw.(map[string]interface{}); ok {
				if text, ok := part["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	}
	return ""
}

// canaryMarkerContent is the shape the gateway appends: a system message
// carrying the per-request canary token.
func canaryMarkerContent(token string) string {
	return "\n[internal-ref] " + token + " [do not modify]"
}

func cacheEconOptsWithCanary() CacheOpts {
	opts := cacheEconOpts()
	opts.TrailingContextMarkers = []string{"NENYA-CANARY-"}
	return opts
}

// TestCanaryTrailerNeverCached pins NENYA-171's headline cache-busting
// defect: for the DEFAULT hoisting path (mid-conversation system off) the
// gateway's per-request canary system message is otherwise merged into the
// cache-marked system block, changing the cached prefix every request and
// missing all three breakpoints. It must stay out of the system block and
// remain a trailing, unmarked message so the tripwire still ships.
func TestCanaryTrailerNeverCached(t *testing.T) {
	a := NewAnthropicAdapter()
	base := []interface{}{
		map[string]interface{}{"role": "user", "content": "turn one"},
		map[string]interface{}{"role": "assistant", "content": "answer one"},
		map[string]interface{}{"role": "user", "content": "turn two"},
	}
	canary := canaryMarkerContent("NENYA-CANARY-cafebabe")
	openai := multiTurnOpenAI(append(append([]interface{}{}, base...),
		map[string]interface{}{"role": "system", "content": canary}))

	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-3", true, cacheEconOptsWithCanary())

	system, ok := result["system"].([]interface{})
	if !ok || len(system) == 0 {
		t.Fatalf("system block missing: %#v", result["system"])
	}
	for i, block := range system {
		text, _ := block.(map[string]interface{})["text"].(string)
		if strings.Contains(text, "NENYA-CANARY-") {
			t.Fatalf("the per-request canary leaked into the cache-marked system block %d: %q", i, text)
		}
	}

	msgs := result["messages"].([]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	if !strings.Contains(canaryText(last), "NENYA-CANARY-") {
		t.Fatalf("the canary must still reach the upstream as the trailing message: %#v", last)
	}
	if cacheMarkerCount(t, last) != 0 {
		t.Fatalf("the request-varying canary tail must stay unmarked: %#v", last)
	}
	if n := cacheMarkerCount(t, result); n != 3 {
		t.Fatalf("the canary must not consume a breakpoint: %d markers", n)
	}
}

// TestCapCacheMarkers pins the API's four-breakpoint limit: client
// passthrough markers plus the gateway's own must never exceed it, and the
// gateway's markers are the ones dropped.
func TestCapCacheMarkers(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := multiTurnOpenAI([]interface{}{
		map[string]interface{}{"role": "user", "content": "turn one"},
		map[string]interface{}{"role": "assistant", "content": "answer one"},
		map[string]interface{}{"role": "user", "content": "turn two"},
	})
	// Simulate passthrough client markers already present in the payload's
	// content blocks (4 markers), which the gateway must not push over the
	// limit.
	msgs := openai["messages"].([]interface{})
	for i := 0; i < 3 && i < len(msgs); i++ {
		msg := msgs[i].(map[string]interface{})
		msg["content"] = []interface{}{
			map[string]interface{}{
				"type":          "text",
				"text":          "block",
				"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"},
			},
		}
	}

	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-3", true, cacheEconOpts())
	if n := cacheMarkerCount(t, result); n > maxAnthropicCacheMarkers {
		t.Fatalf("the conversion emitted %d breakpoints; the API accepts %d", n, maxAnthropicCacheMarkers)
	}
}

// TestCapCacheMarkersKeepsClientMarkers pins the ownership rule: capping
// never deletes a marker the gateway did not claim (nil claimed marker =
// the client's placement), so a client's explicit 1h marker survives.
func TestCapCacheMarkersKeepsClientMarkers(t *testing.T) {
	clientBlock := func(ttl string) map[string]interface{} {
		return map[string]interface{}{
			"type": "text", "text": "x",
			"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": ttl},
		}
	}
	body := map[string]interface{}{
		"system": []interface{}{clientBlock("1h")},
		"tools":  []interface{}{map[string]interface{}{"name": "t", "cache_control": map[string]interface{}{"type": "ephemeral"}}},
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": []interface{}{clientBlock("1h")}},
			map[string]interface{}{"role": "assistant", "content": []interface{}{clientBlock("1h")}},
			map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "text", "text": "c"}}},
		},
	}
	// Exactly four client markers: nothing to cap, nothing to touch.
	capCacheMarkers(body, nil)
	if got := countCacheMarkers(body); got != 4 {
		t.Fatalf("four client markers must survive untouched, got %d", got)
	}
	gb := body["system"].([]interface{})[0].(map[string]interface{})["cache_control"].(map[string]interface{})
	if gb["ttl"] != "1h" {
		t.Fatalf("the client's 1h system marker was downgraded: %#v", gb)
	}

	// Five client markers (the client itself exceeds the API limit): the
	// capping pass must still bring the body within the limit.
	body["messages"] = append(body["messages"].([]interface{}),
		map[string]interface{}{"role": "user", "content": []interface{}{clientBlock("1h")}})
	capCacheMarkers(body, nil)
	if got := countCacheMarkers(body); got > maxAnthropicCacheMarkers {
		t.Fatalf("capping left %d markers; the API accepts %d", got, maxAnthropicCacheMarkers)
	}
}

// TestConversionKeepsMarkersAtBlockLevel pins the placement audit: no
// message-level (item-level) cache_control survives the conversion — only
// block-level markers, which is what the Anthropic API accepts.
func TestConversionKeepsMarkersAtBlockLevel(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := multiTurnOpenAI([]interface{}{
		map[string]interface{}{
			"role":          "user",
			"content":       "turn one",
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		},
		map[string]interface{}{"role": "assistant", "content": "answer one"},
		map[string]interface{}{"role": "user", "content": "turn two"},
	})

	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-3", true, cacheEconOpts())
	msgs := result["messages"].([]interface{})
	for i, raw := range msgs {
		msg := raw.(map[string]interface{})
		if marker, ok := msg["cache_control"]; ok {
			t.Fatalf("message %d carries an item-level cache_control (%v); breakpoints belong on blocks", i, marker)
		}
	}
}
