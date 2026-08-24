package proxy

import "testing"

func TestClassifyStreamHead(t *testing.T) {
	tests := []struct {
		name   string
		chunk  string
		expect streamHeadKind
	}{
		{
			name:   "openai error data",
			chunk:  "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n",
			expect: headError,
		},
		{
			name:   "flat error server_error",
			chunk:  "data: {\"message\":\"Streaming response failed: [502] Upstream error from Nvidia: Service temporarily overloaded\",\"type\":\"server_error\"}\n\n",
			expect: headError,
		},
		{
			name:   "anthropic event error",
			chunk:  "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
			expect: headError,
		},
		{
			name:   "bare json error (non-data)",
			chunk:  "{\"error\":{\"message\":\"boom\",\"type\":\"api_error\"}}\n",
			expect: headError,
		},
		{
			name:   "content chunk first",
			chunk:  "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
			expect: headNormal,
		},
		{
			name:   "content then error (not head error)",
			chunk:  "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"error\":{\"message\":\"boom\"}}\n\n",
			expect: headNormal,
		},
		{
			name:   "done marker",
			chunk:  "data: [DONE]\n\n",
			expect: headNormal,
		},
		{
			name:   "keepalive comment then content",
			chunk:  ": ping\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
			expect: headNormal,
		},
		{
			name:   "empty data keepalive",
			chunk:  "data: \n\n",
			expect: headUndetermined,
		},
		{
			name:   "partial json",
			chunk:  "data: {\"error\":{\"mess",
			expect: headUndetermined,
		},
		{
			name:   "event field then content",
			chunk:  "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hi\"}}\n\n",
			expect: headNormal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStreamHead([]byte(tc.chunk)); got != tc.expect {
				t.Errorf("classifyStreamHead(%q) = %v, want %v", tc.chunk, got, tc.expect)
			}
		})
	}
}
