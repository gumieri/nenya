package mcp

import "encoding/json"

// JSONRPCVersion represents a JSON-RPC protocol version string.
type JSONRPCVersion string

const JSONRPCVersion2 JSONRPCVersion = "2.0"

// Request is a JSON-RPC 2.0 request message.
type Request struct {
	JSONRPC JSONRPCVersion `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  any            `json:"params,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification (no ID, no response expected).
type Notification struct {
	JSONRPC JSONRPCVersion `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  any            `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response message.
type Response struct {
	JSONRPC JSONRPCVersion `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  any            `json:"result,omitempty"`
	Error   *Error         `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return e.Message
}

const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
)

// InitializeParams contains the parameters sent during MCP initialization.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      ImplementationInfo `json:"clientInfo"`
}

type ClientCapabilities struct {
	Roots *RootsCapability `json:"roots,omitempty"`
}

type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ImplementationInfo describes the client or server implementation.
type ImplementationInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ImplementationInfo `json:"serverInfo"`
}

type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema is the tool's declared argument schema. The typed fields are the
// keywords the proxy consumes directly; every other keyword ($ref, $defs,
// items, oneOf, patternProperties, constraints, ...) is preserved verbatim in
// the decoded raw map, so a schema survives the tools/list decode intact
// (NENYA-172) and the sanitizer/argument guard see the whole declaration.
type InputSchema struct {
	Type                 string         `json:"type"`
	Properties           map[string]any `json:"properties,omitempty"`
	Required             []string       `json:"required,omitempty"`
	AdditionalProperties any            `json:"additionalProperties,omitempty"`

	// raw is the decoded schema object with every keyword. Unexported so it
	// never round-trips into a request body on its own.
	raw map[string]any
}

// UnmarshalJSON decodes a tool schema into both the typed fields and the raw
// keyword map, so decoding is no longer lossy.
func (s *InputSchema) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		// Not an object: a malformed schema must not fail the whole tools/list
		// decode (that would drop the server's tools), so it decodes as empty.
		*s = InputSchema{}
		return nil
	}
	// The typed fields are a convenience view; a shape mismatch (a string
	// required list, an array properties map, a numeric type) is ignored rather
	// than failing the decode. The raw map is the authoritative declaration.
	var typed struct {
		Type                 string         `json:"type"`
		Properties           map[string]any `json:"properties,omitempty"`
		Required             []string       `json:"required,omitempty"`
		AdditionalProperties any            `json:"additionalProperties,omitempty"`
	}
	_ = json.Unmarshal(data, &typed)
	*s = InputSchema{
		Type:                 typed.Type,
		Properties:           typed.Properties,
		Required:             typed.Required,
		AdditionalProperties: typed.AdditionalProperties,
		raw:                  raw,
	}
	return nil
}

// MarshalJSON emits the preserved keyword map, so a decode/encode round-trip is
// lossless (a schema built in code falls back to its typed fields).
func (s InputSchema) MarshalJSON() ([]byte, error) {
	if s.raw != nil {
		return json.Marshal(s.raw)
	}
	return json.Marshal(struct {
		Type                 string         `json:"type,omitempty"`
		Properties           map[string]any `json:"properties,omitempty"`
		Required             []string       `json:"required,omitempty"`
		AdditionalProperties any            `json:"additionalProperties,omitempty"`
	}{s.Type, s.Properties, s.Required, s.AdditionalProperties})
}

// AsMap renders the schema as generic JSON, preserving every keyword. It returns
// nil for a schema that declares nothing at all.
func (s InputSchema) AsMap() map[string]any {
	if s.raw != nil {
		return s.raw
	}
	// A schema built in code (not decoded) falls back to its typed fields.
	if s.Type == "" && len(s.Properties) == 0 && len(s.Required) == 0 && s.AdditionalProperties == nil {
		return nil
	}
	encoded, err := json.Marshal(struct {
		Type                 string         `json:"type,omitempty"`
		Properties           map[string]any `json:"properties,omitempty"`
		Required             []string       `json:"required,omitempty"`
		AdditionalProperties any            `json:"additionalProperties,omitempty"`
	}{s.Type, s.Properties, s.Required, s.AdditionalProperties})
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return out
}

type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type CallToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (r *CallToolResult) Text() string {
	var out string
	for _, b := range r.Content {
		if b.Type == "text" {
			if out != "" {
				out += "\n"
			}
			out += b.Text
		}
	}
	return out
}

type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}
