package gateway

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/nenya/internal/tiktoken"
)

func countGW(t *testing.T) *NenyaGateway {
	t.Helper()
	return New(context.Background(), testConfig(), testSecrets(), testLogger())
}

// pngBase64 renders a minimal PNG header of the given pixel size.
func pngBase64(width, height uint32) string {
	b := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	b = append(b, 0, 0, 0, 13, 'I', 'H', 'D', 'R')
	b = append(b, byte(width>>24), byte(width>>16), byte(width>>8), byte(width))
	b = append(b, byte(height>>24), byte(height>>16), byte(height>>8), byte(height))
	return base64.StdEncoding.EncodeToString(b)
}

// TestCountRequestTokens_Images pins NENYA-157's image accounting: an image
// block was counted as the two tokens of the literal "[image]", so a vision
// payload slipped past every budget derived from the estimate.
func TestCountRequestTokens_Images(t *testing.T) {
	gw := countGW(t)
	want := tiktoken.AreaTokens(1000, 800)

	t.Run("openai data url", func(t *testing.T) {
		payload := map[string]interface{}{
			"messages": []interface{}{map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "what is this?"},
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{
						"url": "data:image/png;base64," + pngBase64(1000, 800),
					}},
				},
			}},
		}
		got := gw.CountRequestTokens(payload)
		text := gw.CountTokens("what is this?")
		if wantTotal := text + want + perMessageOverheadTokens; got != wantTotal {
			t.Fatalf("tokens = %d, want %d (text %d + image %d + framing %d)", got, wantTotal, text, want, perMessageOverheadTokens)
		}
	})

	t.Run("openai image_url string form", func(t *testing.T) {
		payload := map[string]interface{}{
			"messages": []interface{}{map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "image_url", "image_url": "data:image/png;base64," + pngBase64(1000, 800)},
				},
			}},
		}
		if got := gw.CountRequestTokens(payload); got != want+perMessageOverheadTokens {
			t.Fatalf("tokens = %d, want %d (image + framing)", got, want+perMessageOverheadTokens)
		}
	})

	t.Run("anthropic base64 block", func(t *testing.T) {
		payload := map[string]interface{}{
			"messages": []interface{}{map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "image", "source": map[string]interface{}{
						"type": "base64", "media_type": "image/png", "data": pngBase64(1000, 800),
					}},
				},
			}},
		}
		if got := gw.CountRequestTokens(payload); got != want+perMessageOverheadTokens {
			t.Fatalf("tokens = %d, want %d (image + framing)", got, want+perMessageOverheadTokens)
		}
	})

	t.Run("remote url falls back to the default", func(t *testing.T) {
		payload := map[string]interface{}{
			"messages": []interface{}{map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{
						"url": "https://example.invalid/cat.png",
					}},
				},
			}},
		}
		if got := gw.CountRequestTokens(payload); got != tiktoken.DefaultImageTokens+perMessageOverheadTokens {
			t.Fatalf("tokens = %d, want %d (default image + framing)", got, tiktoken.DefaultImageTokens+perMessageOverheadTokens)
		}
	})
}

// TestCountRequestTokens_Declarations pins the tool/structured-output cost: the
// declarations are forwarded upstream and rendered by the provider, so they
// must be budgeted, and a bigger schema must cost more.
func TestCountRequestTokens_Declarations(t *testing.T) {
	gw := countGW(t)
	base := map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{"content": "hi"}},
	}
	small := map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{"content": "hi"}},
		"tools": []interface{}{map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": "read_file"},
		}},
	}
	large := map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{"content": "hi"}},
		"tools": []interface{}{map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "read_file",
				"description": strings.Repeat("Reads a file from disk and returns its contents. ", 20),
			},
		}},
	}
	noTools := gw.CountRequestTokens(base)
	withSmall := gw.CountRequestTokens(small)
	withLarge := gw.CountRequestTokens(large)

	if withSmall <= noTools {
		t.Fatalf("a tool declaration must add tokens: %d vs %d", withSmall, noTools)
	}
	if withLarge <= withSmall {
		t.Fatalf("a longer description must add tokens: %d vs %d", withLarge, withSmall)
	}

	structured := map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{"content": "hi"}},
		"response_format": map[string]interface{}{
			"type":        "json_schema",
			"json_schema": map[string]interface{}{"name": "out", "schema": map[string]interface{}{"type": "object"}},
		},
	}
	if got := gw.CountRequestTokens(structured); got <= noTools {
		t.Fatalf("a structured-output schema must add tokens: %d vs %d", got, noTools)
	}
}

// TestCountRequestTokens_ToolCallArguments pins the MCP-loop shape: an
// assistant turn that called a tool carries its arguments in `tool_calls`, not
// in `content`, so counting only content priced such a message at framing alone.
func TestCountRequestTokens_ToolCallArguments(t *testing.T) {
	gw := countGW(t)
	args := `{"path":"/etc/hosts","encoding":"utf-8","offset":0,"limit":200}`
	withCall := map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []interface{}{map[string]interface{}{
				"id": "call_1", "type": "function",
				"function": map[string]interface{}{"name": "read_file", "arguments": args},
			}},
		}},
	}
	empty := map[string]interface{}{
		"messages": []interface{}{map[string]interface{}{"role": "assistant", "content": nil}},
	}
	got, base := gw.CountRequestTokens(withCall), gw.CountRequestTokens(empty)
	if got <= base {
		t.Fatalf("tool-call arguments must be counted: %d vs %d", got, base)
	}
	want := base + gw.CountTokens("read_file"+args)
	if got != want {
		t.Fatalf("tokens = %d, want %d (framing + name + arguments)", got, want)
	}
}

// TestCountRequestTokens_DeclarationsWithoutMessages pins the zero-estimate
// hole: a payload with no messages still carries declarations upstream, so it
// must not estimate zero.
func TestCountRequestTokens_DeclarationsWithoutMessages(t *testing.T) {
	gw := countGW(t)
	payload := map[string]interface{}{
		"tools": []interface{}{map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": "read_file"},
		}},
	}
	if got := gw.CountRequestTokens(payload); got <= 0 {
		t.Fatalf("a declaration-only payload must still be priced, got %d", got)
	}
}

// TestCountRequestTokens_EstimateComposition pins the shape of the estimate:
// for a realistic request the composed value must stay within a factor of two
// of the raw cl100k count of the same content — framing and declarations add
// overhead, they must not multiply the estimate (NENYA-157's consistency
// requirement; the calibration loop then corrects the residual per model).
func TestCountRequestTokens_EstimateComposition(t *testing.T) {
	gw := countGW(t)
	text := strings.Repeat("Please summarize the following code and explain the tradeoffs. ", 20)
	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are a helpful assistant."},
			map[string]interface{}{"role": "user", "content": text},
		},
		"tools": []interface{}{map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "read_file",
				"description": "Read a file",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}},
			},
		}},
	}
	// Exact composition: message text + framing + the serialized declaration
	// (plus its framing). A regression in any term breaks this.
	// The implementation counts one concatenated text buffer, so the expected
	// content term is the BPE of that concatenation (token counts are not
	// additive across a join).
	content := gw.CountTokens("You are a helpful assistant." + text)
	declaration := gw.CountTokens(marshalForCount(payload["tools"].([]interface{})[0]))
	want := content + 2*perMessageOverheadTokens + declaration + perDeclarationOverheadTokens
	got := gw.CountRequestTokens(payload)
	if got != want {
		t.Fatalf("composed estimate = %d, want %d (content %d + framing %d + declaration %d + %d)",
			got, want, content, 2*perMessageOverheadTokens, declaration, perDeclarationOverheadTokens)
	}
}
