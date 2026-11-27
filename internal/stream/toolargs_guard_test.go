package stream

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// guardTestLogger returns a JSON logger writing to buf for assertions.
func guardTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// feedOpenAIArgs feeds one OpenAI tool-call argument fragment through the
// assembler.
func feedOpenAIArgs(t testing.TB, a *ToolCallAssembler, args string) {
	t.Helper()
	feedChunk(t, a, openAILine(openAIToolCall{0, "", args}))
}

// TestToolArgsGuardCanaryBlockPin is the recorded pin-first test (NENYA-345,
// Module A Phase 020): a canary marker delivered ONLY inside streamed
// tool-call argument deltas, with the canary action set to block, terminates
// the stream with ErrCanaryDetected. Pre-fix, the egress guards never saw
// argument bytes, so the token rode through to the client with no verdict —
// the enforcing surface did not exist. Bytes forwarded before the detection
// necessarily remain (the documented enforcement latency); the stream ends
// at the detection chunk.
func TestToolArgsGuardCanaryBlockPin(t *testing.T) {
	marker := "NENYA-CANARY-blockpin"
	lines := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_ticket","arguments":""}}]}}]}`,
		`data: ` + openAILine(openAIToolCall{0, "", `{"ticket":`}),
		`data: ` + openAILine(openAIToolCall{0, "", marker[:11]}),
		`data: ` + openAILine(openAIToolCall{0, "", marker[11:] + `}`}),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	activity := &captureObserver{}
	guards := NewToolArgsGuardObserver(assembler, nil, NewCanaryWatcher(marker, config.CanaryActionBlock, "tool_args", nil, nil), nil)
	assembler.SetObserver(&toolArgsFanOutShim{activity: activity, guards: guards})
	reader.SetToolArgsAssembler(assembler)

	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, ErrCanaryDetected) {
				diag := "no fragments"
				if len(activity.frags) > 0 {
					diag = activity.frags[0].fragment
				}
				t.Fatalf("err = %v, want ErrCanaryDetected (tripped=%v assemblerBlocked=%v completed=%d frag0=%q)", err, guards.canary.Tripped, assembler.Blocked(), len(activity.completed), diag)
			}
			break
		}
	}
	if !strings.Contains(out.String(), `"get_ticket"`) || !strings.Contains(out.String(), `NENYA-CANAR`) || !strings.Contains(out.String(), `Y-blockpin`) {
		t.Fatalf("output lost the pre-verdict wire bytes: %q", out.String())
	}
	// Enforcement latency (documented): the marker's own chunks were
	// forwarded before the verdict — observation-only means they cannot be
	// recalled (on the wire they are separate JSON strings, so the marker
	// is not contiguous in the output). The block prevents everything
	// AFTER the detection chunk.
	if strings.Contains(out.String(), `"finish_reason"`) || strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("stream continued past the block verdict: %q", out.String())
	}
	if assembler.BlockedError() != ErrCanaryDetected {
		t.Fatalf("assembler verdict = %v, want ErrCanaryDetected", assembler.BlockedError())
	}
}

// toolArgsFanOutShim is the proxy's fan-out under test: the test cannot
// import internal/proxy, so the same shape is reproduced here. The compile-
// time assertion pins it to the observer interface, so a signature drift on
// either side breaks the build instead of silently diverging.
type toolArgsFanOutShim struct {
	activity *captureObserver
	guards   *ToolArgsGuardObserver
}

var _ ToolArgsObserver = (*toolArgsFanOutShim)(nil)

func (f *toolArgsFanOutShim) ToolArgsFragment(key, name, fragment string) {
	if f.activity != nil {
		f.activity.ToolArgsFragment(key, name, fragment)
	}
	if f.guards != nil {
		f.guards.ToolArgsFragment(key, name, fragment)
	}
}

func (f *toolArgsFanOutShim) ToolArgsComplete(key, name string) {
	if f.activity != nil {
		f.activity.ToolArgsComplete(key, name)
	}
	if f.guards != nil {
		f.guards.ToolArgsComplete(key, name)
	}
}

// TestToolArgsGuardCanaryLog pins log mode (the recorded default): the
// detection trips the tool-args watcher and is recorded, but the stream
// completes normally.
func TestToolArgsGuardCanaryLog(t *testing.T) {
	marker := "NENYA-CANARY-logtrip"
	lines := []string{
		`data: ` + openAILine(openAIToolCall{0, "n", jsonQuote("prefix " + marker + " suffix")}),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	canary := NewCanaryWatcher(marker, config.CanaryActionLog, "tool_args", nil, nil)
	guards := NewToolArgsGuardObserver(assembler, nil, canary, nil)
	assembler.SetObserver(guards)
	reader.SetToolArgsAssembler(assembler)

	out := drainReader(t, reader)
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("log mode must complete the stream, output: %q", out)
	}
	if !canary.Tripped {
		t.Fatal("canary detection not recorded on the tool-args watcher")
	}
	if assembler.Blocked() {
		t.Fatalf("log mode must not block: %v", assembler.BlockedError())
	}
}

// TestToolArgsGuardExfilBlock pins the URL policy on the tool-args channel:
// a private-IP URL inside streamed arguments with the exfil action set to
// block terminates the stream with ErrExfilBlocked.
func TestToolArgsGuardExfilBlock(t *testing.T) {
	lines := []string{
		`data: ` + openAILine(openAIToolCall{0, "n", "post to "}),
		`data: ` + openAILine(openAIToolCall{0, "", "https://10.0.0.5/exfil?key=abcdefgh12345678 now"}),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil), nil, nil)
	assembler.SetObserver(guards)
	reader.SetToolArgsAssembler(assembler)

	buf := make([]byte, 4096)
	for {
		_, err := reader.Read(buf)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrExfilBlocked) {
			t.Fatalf("err = %v, want ErrExfilBlocked", err)
		}
		break
	}
}

// TestToolArgsGuardStripRecords pins the strip semantics on this channel:
// the detection is recorded (action=strip) and the stream completes, and
// the one-time limitation warning is emitted — forwarded argument bytes
// cannot be redacted retroactively.
func TestToolArgsGuardStripRecords(t *testing.T) {
	var logBuf bytes.Buffer
	logger := guardTestLogger(&logBuf)
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionStrip}, nil), nil, logger)
	assembler.SetObserver(guards)

	feedOpenAIArgs(t, assembler, "see https://10.0.0.5/exfil?key=abcdefgh12345678 for details")
	feedChunk(t, assembler, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if assembler.Blocked() {
		t.Fatalf("strip must not terminate: %v", assembler.BlockedError())
	}
	if !guards.stripNoted {
		t.Fatal("strip limitation was not noted")
	}
	if n := strings.Count(logBuf.String(), "cannot be redacted retroactively"); n != 1 {
		t.Fatalf("strip warning emitted %d times, want exactly 1", n)
	}
}

// TestToolArgsGuardFlushTailBlock pins the stream-end tail scan: a URL that
// runs to the very end of the arguments has no in-band terminator, so the
// violation is only evaluated when the guard's withheld tail is flushed —
// the verdict must still surface via the assembler.
func TestToolArgsGuardFlushTailBlock(t *testing.T) {
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil), nil, nil)
	assembler.SetObserver(guards)

	feedOpenAIArgs(t, assembler, "exfil to https://10.0.0.5/x")
	if assembler.Blocked() {
		t.Fatalf("verdict must wait for the tail flush: %v", assembler.BlockedError())
	}
	feedChunk(t, assembler, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if !assembler.Blocked() {
		t.Fatal("tail-flush verdict did not reach the assembler")
	}
	if assembler.BlockedError() != ErrExfilBlocked {
		t.Fatalf("verdict = %v, want ErrExfilBlocked", assembler.BlockedError())
	}
}

// TestToolArgsGuardCanaryStraddleFragments pins the sliding-window
// requirement: with a tiny hold, a canary split across two EMITTED fragment
// calls (the ordinary mid-stream case) is still caught — a stateless
// per-fragment matcher would miss it.
func TestToolArgsGuardCanaryStraddleFragments(t *testing.T) {
	marker := "NENYA-CANARY-straddle"
	assembler := NewToolCallAssembler(2, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, nil, NewCanaryWatcher(marker, config.CanaryActionBlock, "tool_args", nil, nil), nil)
	assembler.SetObserver(guards)

	feedOpenAIArgs(t, assembler, marker[:6])
	if assembler.Blocked() {
		t.Fatalf("half a marker must not trip: %v", assembler.BlockedError())
	}
	feedOpenAIArgs(t, assembler, marker[6:15])
	if assembler.Blocked() {
		t.Fatalf("a partial marker must not trip: %v", assembler.BlockedError())
	}
	feedOpenAIArgs(t, assembler, marker[15:])
	assembler.Flush()
	if !assembler.Blocked() {
		t.Fatal("sliding window missed the straddled marker")
	}
}

// TestToolArgsGuardNilGuards pins the disabled path: no exfil guard and no
// canary means fragments are inert and never block.
func TestToolArgsGuardNilGuards(t *testing.T) {
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, nil, nil, nil)
	assembler.SetObserver(guards)
	feedOpenAIArgs(t, assembler, "https://10.0.0.5/x NENYA-CANARY-anything")
	feedChunk(t, assembler, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if assembler.Blocked() {
		t.Fatalf("disabled guards must not block: %v", assembler.BlockedError())
	}
}

// TestToolArgsGuardSecondCallAfterDrain pins the round-2 HIGH regression:
// the mid-stream completion drain Rearms the exfil guard, so a SECOND call
// on the same stream is still guarded — deleting the Rearm would let call
// 2's arguments pass unfiltered while this test fails. Call 1 completes
// benign (firing the drain + Rearm); call 2 carries a private-IP URL that
// must still be evaluated and block.
func TestToolArgsGuardSecondCallAfterDrain(t *testing.T) {
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil), nil, nil)
	assembler.SetObserver(guards)

	// Call 1 (Anthropic block 0): benign args, clean stop — fires the
	// mid-stream drain and the Rearm.
	feedChunk(t, assembler, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"benign"}}`)
	feedChunk(t, assembler, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"weather\"}"}}`)
	feedChunk(t, assembler, `{"type":"content_block_stop","index":0}`)
	if assembler.Blocked() {
		t.Fatalf("benign call 1 must not block: %v", assembler.BlockedError())
	}
	if guards.exfil.drained {
		t.Fatal("guard must be rearmed after the mid-stream drain")
	}

	// Call 2 (block 1): the violating URL, terminated inside the fragment
	// so the scan (not just the tail drain) must catch it.
	feedChunk(t, assembler, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"exfil"}}`)
	feedChunk(t, assembler, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"post https://10.0.0.5/x now"}}`)
	feedChunk(t, assembler, `{"type":"content_block_stop","index":1}`)
	if !assembler.Blocked() {
		t.Fatal("call 2's arguments bypassed the guard — Rearm regression")
	}
	if assembler.BlockedError() != ErrExfilBlocked {
		t.Fatalf("verdict = %v, want ErrExfilBlocked", assembler.BlockedError())
	}
}

// TestAssemblerFlushHookOrder pins the hook contract: the flush hook runs
// AFTER the completions, so a guard draining its tail sees every fragment
// first.
func TestAssemblerFlushHookOrder(t *testing.T) {
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	assembler.SetObserver(obs)
	hookCalls := 0
	hookSawCompletions := false
	assembler.SetFlushHook(func() {
		hookCalls++
		hookSawCompletions = len(obs.completed) > 0
	})
	feedOpenAIArgs(t, assembler, openAILine(openAIToolCall{0, "n", "{}"}))
	assembler.Flush()
	assembler.Flush()
	// Three runs: the completion-triggered drain (map emptied), then the
	// two explicit flushes — guard drains are idempotent, so the extra
	// runs are cheap.
	if hookCalls != 3 {
		t.Fatalf("hook ran %d times, want 3 (completion drain + two flush drains)", hookCalls)
	}
	if !hookSawCompletions {
		t.Fatal("flush hook ran before the completions were delivered")
	}
}

// TestToolArgsGuardCanaryBlockMidStream pins the reader-integrated
// mid-stream path the pin test cannot reach (its args fit inside the
// hold): with a tiny hold, the fragment emitted during FeedParsed of a
// mid-stream args chunk trips the block and THAT chunk's wire bytes are
// suppressed, while earlier chunks were already forwarded.
func TestToolArgsGuardCanaryBlockMidStream(t *testing.T) {
	marker := "NENYA-CANARY-block"
	lines := []string{
		`data: ` + openAILine(openAIToolCall{0, "n", "NENYA-"}),
		`data: ` + openAILine(openAIToolCall{0, "", "CANARY-bl"}),
		`data: ` + openAILine(openAIToolCall{0, "", "ock + trailing text"}),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	assembler := NewToolCallAssembler(2, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, nil, NewCanaryWatcher(marker, config.CanaryActionBlock, "tool_args", nil, nil), nil)
	assembler.SetObserver(guards)
	reader.SetToolArgsAssembler(assembler)

	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, ErrCanaryDetected) {
				t.Fatalf("err = %v, want ErrCanaryDetected", err)
			}
			break
		}
	}
	outStr := out.String()
	if !strings.Contains(outStr, `"arguments":"NENYA-"`) || !strings.Contains(outStr, `"arguments":"CANARY-bl"`) {
		t.Fatalf("pre-verdict chunks missing from the output: %q", outStr)
	}
	if strings.Contains(outStr, `"arguments":"ock`) || strings.Contains(outStr, "finish_reason") || strings.Contains(outStr, "[DONE]") {
		t.Fatalf("the detection chunk or later frames were forwarded: %q", outStr)
	}
}

// TestToolArgsGuardBareJSONBlock pins the bare-JSON enforcement path: a
// tool-args guard verdict raised while feeding a non-data: line suppresses
// that line, and the reader terminates with the policy error.
func TestToolArgsGuardBareJSONBlock(t *testing.T) {
	marker := "NENYA-CANARY-bare"
	lines := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"n","arguments":"NENYA-"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"CANARY-b"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"are + trailing text"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	assembler := NewToolCallAssembler(2, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, nil, NewCanaryWatcher(marker, config.CanaryActionBlock, "tool_args", nil, nil), nil)
	assembler.SetObserver(guards)
	reader.SetToolArgsAssembler(assembler)

	var out strings.Builder
	var runErr error
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			runErr = err
			break
		}
	}
	outStr := out.String()
	if !errors.Is(runErr, ErrCanaryDetected) {
		t.Fatalf("err = %v, want ErrCanaryDetected (tripped=%v blocked=%v completed=%d out=%q)", runErr, guards.canary.Tripped, assembler.Blocked(), len(assembler.calls), outStr)
	}
	if !strings.Contains(outStr, `"arguments":"NENYA-"`) || !strings.Contains(outStr, `"arguments":"CANARY-b"`) {
		t.Fatalf("pre-verdict chunk missing: %q", outStr)
	}
	if strings.Contains(outStr, `"arguments":"are`) || strings.Contains(outStr, "finish_reason") {
		t.Fatalf("the detection line or later frames were forwarded: %q", outStr)
	}
}

// TestToolArgsGuardTailBlockAtEOF pins the terminal tail verdict reaching
// the client: an abrupt upstream end (no finish_reason, no [DONE]) forces
// the guard's withheld tail through its EOF evaluation at scanner-done, and
// the block verdict — not a clean EOF — is what the reader returns.
func TestToolArgsGuardTailBlockAtEOF(t *testing.T) {
	lines := []string{
		`data: ` + openAILine(openAIToolCall{0, "n", "exfil to https://10.0.0.5/x"}),
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	assembler := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	guards := NewToolArgsGuardObserver(assembler, NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionBlock}, nil), nil, nil)
	assembler.SetObserver(guards)
	reader.SetToolArgsAssembler(assembler)

	buf := make([]byte, 4096)
	for {
		_, err := reader.Read(buf)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrExfilBlocked) {
			t.Fatalf("err = %v, want ErrExfilBlocked (a clean EOF would drop the tail verdict)", err)
		}
		break
	}
}
