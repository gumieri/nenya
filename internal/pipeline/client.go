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
	substring  string
	name       string
	isIDE      bool
}

var clientPatterns = []clientPattern{
	{substring: "cursor", name: "cursor", isIDE: true},
	{substring: "opencode", name: "opencode", isIDE: true},
}

func ClassifyClient(headers http.Header) ClientProfile {
	ua := strings.ToLower(headers.Get("User-Agent"))
	for _, p := range clientPatterns {
		if strings.Contains(ua, p.substring) {
			return ClientProfile{IsIDE: p.isIDE, ClientName: p.name}
		}
	}
	return ClientProfile{}
}
