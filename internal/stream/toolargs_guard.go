package stream

import (
	"log/slog"

	"github.com/nenya/config"
)

// ToolArgsGuardObserver is the Phase 020 enforcing observer (NENYA-345,
// Module A Phase 020): it feeds reassembled tool-call arguments through
// dedicated ExfilGuard and CanaryWatcher instances so the egress policy and
// the canary tripwire cover the tool-args channel. The guard instances are
// SEPARATE from the text-path guards — both keep sliding windows over the
// bytes they have seen, and the tool-args window must hold argument bytes,
// not text. One window is shared across ALL of a stream's tool-call keys:
// different calls' bytes concatenate in it, so an unterminated construct at
// one call's end can merge with the next call's opening bytes and be
// evaluated as one construct — biased toward over-detection in block mode,
// detection-preserving for a deliberately split marker or URL.
//
// Action semantics on this channel (the operator's existing action config
// applies; the recorded default is log):
//
//   - log (default): detections are recorded (metrics channel tool_args +
//     the guards' own logs); the stream continues.
//   - block: the first verdict terminates the stream mid-detection — the
//     assembler is flagged (Blocked/BlockedError) and the reader reports
//     ErrCanaryDetected / ErrExfilBlocked through the existing blocked
//     machinery. The detection chunk and everything after it are
//     suppressed; bytes of the construct forwarded in earlier chunks (up
//     to the guard's withheld tail) cannot be recalled. When the verdict
//     lands at scanner-done (the guard's tail EOF-evaluation during
//     handleScannerDone), it becomes the stream's terminal error; on a
//     canceled request the verdict is recorded but ctx.Err() is returned
//     (the client is gone). Terminal framing already forwarded before the
//     verdict — a [DONE] or an injected gateway_error — precedes it, so a
//     strict client may only see the interruption; the verdict is always
//     recorded.
//   - strip: detections are recorded but nothing is redacted — the
//     reassembled bytes were already forwarded by the time the guard sees
//     them, so retroactive redaction of the wire is impossible; a one-time
//     warning states this. An operator who needs argument content kept from
//     the client uses block.
type ToolArgsGuardObserver struct {
	exfil  *ExfilGuard
	canary *CanaryWatcher
	// assembler receives the block verdicts and the flush hook.
	assembler *ToolCallAssembler
	logger    *slog.Logger
	// stripNoted dedupes the one-time strip-semantics warning.
	stripNoted bool
}

// NewToolArgsGuardObserver builds the enforcing observer and registers the
// assembler's flush hook so the exfil guard's withheld tail is drained at
// stream end (a URL running to the very end of the arguments is evaluated
// with EOF standing in for its terminator, and a tail verdict still raises
// Blocked). Nil exfil/canary disable the respective guard; a nil assembler
// makes the observer detection-recording only (no enforcement path).
func NewToolArgsGuardObserver(assembler *ToolCallAssembler, exfil *ExfilGuard, canary *CanaryWatcher, logger *slog.Logger) *ToolArgsGuardObserver {
	o := &ToolArgsGuardObserver{
		exfil:     exfil,
		canary:    canary,
		assembler: assembler,
		logger:    logger,
	}
	if assembler != nil {
		assembler.SetFlushHook(o.Flush)
	}
	return o
}

// ToolArgsFragment feeds one reassembled fragment through the guards. The
// canary runs first (mirroring the reader's block precedence), then the
// URL policy. Strip-mode returns are not applicable on this channel: the
// fragment was already forwarded, so the detection is recorded and the
// stream continues.
func (o *ToolArgsGuardObserver) ToolArgsFragment(key, name, fragment string) {
	if o == nil || fragment == "" {
		return
	}
	// First verdict wins: after a block, later fragments of the same chunk
	// (and any feed before the reader acts on the verdict) skip the guard
	// work.
	if o.assembler.Blocked() {
		return
	}
	if o.canary != nil {
		if _, action, _ := o.canary.FilterContent(fragment); action == ActionBlock {
			o.assembler.setBlocked(ErrCanaryDetected)
			return
		}
	}
	if o.exfil != nil {
		_, action, _ := o.exfil.FilterContent(fragment)
		switch action {
		case ActionBlock:
			o.assembler.setBlocked(ErrExfilBlocked)
			return
		case ActionRedact:
			// Strip mode: the detection is recorded by the guard; the
			// spliced return cannot be applied to already-forwarded
			// bytes. State the limitation once.
			o.noteStrip()
		}
	}
}

// ToolArgsComplete is a no-op: the guards are stateful windows fed per
// fragment; completion carries no bytes.
func (o *ToolArgsGuardObserver) ToolArgsComplete(key, name string) {}

// Flush drains the exfil guard's withheld tail at stream end — its final
// scan uses EOF standing in for the URL terminator, so a construct that
// ran to the end of the arguments is still evaluated. A tail verdict flags
// the assembler; at scanner-done the stream is ending regardless, and the
// detection is recorded by the guard. Rearm follows unconditionally (the
// text path does the same after every mid-stream drain): the hook also
// fires mid-stream when the last observed call completes, and without it
// the drained guard would pass every later call's arguments unfiltered. A
// raised block verdict survives Rearm (it clears only the drained flag).
func (o *ToolArgsGuardObserver) Flush() {
	if o == nil || o.exfil == nil {
		return
	}
	if _, action := o.exfil.Flush(); action == ActionBlock {
		o.assembler.setBlocked(ErrExfilBlocked)
	}
	o.exfil.Rearm()
}

// noteStrip warns once that strip cannot rewrite forwarded argument bytes.
func (o *ToolArgsGuardObserver) noteStrip() {
	if o.stripNoted {
		return
	}
	o.stripNoted = true
	if o.logger != nil {
		o.logger.Warn("exfil strip requested on the streamed tool-args channel: forwarded argument bytes cannot be redacted retroactively; detections are recorded — use action=block to keep content from the client",
			"channel", "tool_args", "action", config.ExfilActionStrip)
	}
}
