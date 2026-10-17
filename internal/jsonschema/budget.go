package jsonschema

import (
	"encoding/json"
	"fmt"
)

// Input-budget caps (NENYA-199). A client-supplied schema is walked BEFORE it
// is sanitized or a request body is converted, so an adversarial declaration
// is rejected with a clear client error instead of being decoded, sanitized,
// re-serialized and forwarded — Sanitize already degrades a hostile schema to
// a valid one, but only after paying for it. The caps sit far above what a
// hand-written or code-generated tool declaration needs and above the
// sanitizer's OUTPUT caps (maxDepth/maxNodes), so the budget rejects input
// that is adversarial rather than merely large. They are deliberately
// constants rather than configuration: a resource guard, not a policy knob.
const (
	// MaxInputNodes caps the values (containers and scalars, not keys) of one
	// schema.
	MaxInputNodes = 50000
	// MaxPayloadNodes caps the sum of the per-tool node counts, so a payload
	// cannot multiply the per-schema allowance by its tool count.
	MaxPayloadNodes = 4 * MaxInputNodes
	// MaxInputDepth caps the nesting depth of one schema.
	MaxInputDepth = 100
	// MaxInputBytes caps the serialized size of one schema.
	MaxInputBytes = 1 << 20
)

// BudgetError reports an input-budget violation of a client-supplied schema.
type BudgetError struct {
	// Tool is the declared tool name, when one can be determined.
	Tool string
	// Dimension is the exceeded budget, one of the closed set "nodes"
	// (per schema), "payload_nodes" (the aggregate across a tool list),
	// "depth", or "bytes".
	Dimension string
	// Limit is the exceeded cap.
	Limit int
	// Observed is the measured value. The walk dimensions report the value at
	// the crossing, and the early bytes rejection reports a lower bound (the
	// raw string bytes visited so far) — every case is over the limit.
	Observed int
}

// Error implements error. The tool name is truncated rune-safely: it comes from
// the client, so it must not inflate the message, and a byte-wise cut could
// emit invalid UTF-8.
func (e *BudgetError) Error() string {
	subject := "schema"
	if e.Tool != "" {
		name := e.Tool
		if runes := []rune(name); len(runes) > maxToolNameLen {
			name = string(runes[:maxToolNameLen]) + "…"
		}
		subject = fmt.Sprintf("tool %q schema", name)
	}
	return fmt.Sprintf("%s exceeds the input budget: %s limit %d (observed %d)",
		subject, e.Dimension, e.Limit, e.Observed)
}

// budgetFrame is one pending value in the budget walk.
type budgetFrame struct {
	value any
	depth int
}

// CheckToolListBudget validates a decoded list of tool declarations against
// the input budget: every tool's argument schema must fit the per-schema caps,
// and the list as a whole must fit the aggregate node cap. A tool in an unexpected
// shape is skipped — this is a resource guard, not a schema validator. The
// returned error is a *BudgetError naming the tool and the exceeded dimension.
func CheckToolListBudget(tools []any) error {
	total := 0
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		name, schema, ok := toolSchema(tool)
		if !ok {
			continue
		}
		nodes, err := walkSchemaBudget(name, schema, MaxInputNodes)
		if err != nil {
			return err
		}
		total += nodes
		if total > MaxPayloadNodes {
			// A distinct dimension from a single oversized schema, so the
			// rejection metric can tell tool-count multiplication from one
			// hostile declaration.
			return &BudgetError{Tool: name, Dimension: "payload_nodes", Limit: MaxPayloadNodes, Observed: total}
		}
	}
	return nil
}

// CheckResponseFormatBudget validates the structured-output schema of an
// OpenAI `response_format` object (`{"type":"json_schema","json_schema":
// {"name":...,"schema":...}}`), the other client-supplied schema surface the
// gateway forwards. Absent or unrecognized shapes pass.
func CheckResponseFormatBudget(responseFormat map[string]any) error {
	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	if !ok {
		return nil
	}
	schema, ok := jsonSchema["schema"]
	if !ok {
		return nil
	}
	name, _ := jsonSchema["name"].(string)
	return CheckSchemaBudget(name, schema)
}

// CheckSchemaBudget enforces the input budget on one decoded schema. The walk
// is iterative on an explicit stack (no recursion), counts containers and
// scalars without allocating per value, and rejects an oversized container in
// O(1) before pushing its elements — so its own bookkeeping is bounded by the
// node cap, and a hostile deep/wide schema is refused after a bounded walk
// instead of a sanitize, a re-serialize and an upstream dispatch.
func CheckSchemaBudget(toolName string, schema any) error {
	_, err := walkSchemaBudget(toolName, schema, MaxInputNodes)
	return err
}

// walkSchemaBudget walks one schema and returns the number of values it
// visited, so the caller can charge them against the payload-level budget. It
// reports the first violation as a *BudgetError. Two caveats on the reported
// numbers: when an input violates more than one dimension the walk reports the
// first it reaches, and when a single container alone exceeds the node budget
// the returned count is what was visited while the error's Observed is the
// projected total for the container (a lower bound on the real total). The
// input is refused either way.
func walkSchemaBudget(toolName string, schema any, nodeLimit int) (int, error) {
	stack := []budgetFrame{{value: schema}}
	nodes, rawBytes := 0, 0

	for len(stack) > 0 {
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		nodes++
		if nodes > nodeLimit {
			return nodes, &BudgetError{Tool: toolName, Dimension: "nodes", Limit: nodeLimit, Observed: nodes}
		}
		if text, ok := frame.value.(string); ok {
			// The encoded size of a string is at least its raw bytes, so
			// tracking them rejects a schema carrying megabytes of text
			// without walking the rest of it.
			rawBytes += len(text)
			if rawBytes > MaxInputBytes {
				return nodes, &BudgetError{Tool: toolName, Dimension: "bytes", Limit: MaxInputBytes, Observed: rawBytes}
			}
			continue
		}

		children, keyBytes, isContainer := containerParts(frame.value)
		if !isContainer {
			// A bool, number, or null: a leaf with no budget of its own.
			continue
		}
		// Account for the whole container before pushing it, so the stack can
		// never grow past the node budget (an oversized container is refused
		// without a frame per element).
		if accounted := nodes + len(stack) + children; accounted > nodeLimit {
			return nodes, &BudgetError{Tool: toolName, Dimension: "nodes", Limit: nodeLimit, Observed: accounted}
		}
		// A container with no children declares nothing deeper, so only a
		// container that actually pushes values is depth-checked (an empty
		// container at the limit is legal).
		childDepth := frame.depth + 1
		if children > 0 && childDepth > MaxInputDepth {
			return nodes, &BudgetError{Tool: toolName, Dimension: "depth", Limit: MaxInputDepth, Observed: childDepth}
		}
		rawBytes += keyBytes
		if rawBytes > MaxInputBytes {
			return nodes, &BudgetError{Tool: toolName, Dimension: "bytes", Limit: MaxInputBytes, Observed: rawBytes}
		}
		stack = appendChildren(stack, frame.value, childDepth)
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		// Unreachable for values produced by json.Unmarshal (string, float64,
		// bool, nil, map, slice): kept defensive, and the caller treats a
		// non-BudgetError as a server-side fault.
		return nodes, fmt.Errorf("schema is not encodable: %w", err)
	}
	if len(encoded) > MaxInputBytes {
		return nodes, &BudgetError{Tool: toolName, Dimension: "bytes", Limit: MaxInputBytes, Observed: len(encoded)}
	}
	return nodes, nil
}

// containerParts reports whether a value is a container, how many children it
// has, and the byte length of its keys (0 for a list).
func containerParts(value any) (children, keyBytes int, ok bool) {
	switch v := value.(type) {
	case map[string]any:
		for key := range v {
			keyBytes += len(key)
		}
		return len(v), keyBytes, true
	case []any:
		return len(v), 0, true
	default:
		return 0, 0, false
	}
}

// appendChildren pushes a container's children onto the stack at childDepth.
func appendChildren(stack []budgetFrame, value any, childDepth int) []budgetFrame {
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			stack = append(stack, budgetFrame{value: child, depth: childDepth})
		}
	case []any:
		for _, child := range v {
			stack = append(stack, budgetFrame{value: child, depth: childDepth})
		}
	}
	return stack
}

// toolSchema returns the tool's name and argument schema, in either wire
// shape: an OpenAI declaration carries them at function.name/function.
// parameters, an Anthropic-shaped one at name/input_schema.
func toolSchema(tool map[string]any) (string, any, bool) {
	if fn, ok := tool["function"].(map[string]any); ok {
		name, _ := fn["name"].(string)
		return name, fn["parameters"], true
	}
	if schema, ok := tool["input_schema"]; ok {
		name, _ := tool["name"].(string)
		return name, schema, true
	}
	return "", nil, false
}
