// Package stream implements Server-Sent Events (SSE) parsing, transformation,
// and filtering. The SSETransformingReader applies provider-specific response
// mutations in real time (e.g. Gemini tool_calls normalization), while the
// StreamFilter performs output-side secret redaction on the streamed response.
package stream
