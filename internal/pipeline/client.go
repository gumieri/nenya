package pipeline

import (
	"net/http"
	"strings"
)

type ClientProfile struct {
	IsIDE      bool
	ClientName string
}

type clientPattern struct {
	header    string
	substring string
	name      string
	isIDE     bool
}

var clientPatterns = []clientPattern{
	{header: "User-Agent", substring: "cursor", name: "cursor", isIDE: true},
	{header: "User-Agent", substring: "opencode", name: "opencode", isIDE: true},
	{header: "Editor-Version", substring: "opencode", name: "opencode", isIDE: true},
	{header: "Editor-Plugin-Version", substring: "opencode", name: "opencode", isIDE: true},
	{header: "User-Agent", substring: "claude-code", name: "claude-code", isIDE: true},
	{header: "Editor-Plugin-Version", substring: "claude-code", name: "claude-code", isIDE: true},
}

func ClassifyClient(headers http.Header) ClientProfile {
	for _, p := range clientPatterns {
		val := strings.ToLower(headers.Get(p.header))
		if val != "" && strings.Contains(val, p.substring) {
			return ClientProfile{IsIDE: p.isIDE, ClientName: p.name}
		}
	}
	return ClientProfile{}
}
