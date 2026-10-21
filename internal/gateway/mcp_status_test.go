package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/testutil"
)

// An unreachable MCP server URL must surface, not vanish: the discovery status
// is failed with the last error and the failure counter increments (NENYA-152).
// gateway.New runs buildMCPToolIndex synchronously, so construction is the
// discovery attempt; 127.0.0.1:1 refuses instantly.
func TestMCPServerStatusFailedDiscovery(t *testing.T) {
	logger := testutil.NewTestLogger()
	cfg := testutil.MinimalConfig()
	cfg.MCPServers = map[string]config.MCPServerConfig{
		"dead": {URL: "http://127.0.0.1:1/sse"},
	}

	gw := New(t.Context(), *cfg, &config.SecretsConfig{ClientToken: "nk-mcpstatus-0123456789"}, logger)

	snap := gw.MCPServerStatusSnapshot()
	entry, ok := snap["dead"]
	if !ok {
		t.Fatalf("the dead server must have a status entry: %+v", snap)
	}
	if entry.Status != MCPServerStatusFailed {
		t.Fatalf("status = %q, want failed", entry.Status)
	}
	if entry.LastError == "" {
		t.Fatal("the failure must carry the last error")
	}
	if entry.Since.IsZero() || time.Since(entry.Since) > time.Minute {
		t.Fatalf("status_since must be fresh: %+v", entry)
	}

	var sb strings.Builder
	gw.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_mcp_server_status_total{server="dead", status="failed"} 1`) {
		t.Fatalf("the failure must be counted:\n%s", sb.String())
	}
}

// A successful discovery records ok (the counter and the status flip together).
func TestMCPServerStatusOKAfterSuccess(t *testing.T) {
	gw := &NenyaGateway{}
	gw.SetMCPServerStatus("srv", MCPServerStatusFailed, errTestFailure{})
	gw.SetMCPServerStatus("srv", MCPServerStatusOK, nil)

	snap := gw.MCPServerStatusSnapshot()
	entry := snap["srv"]
	if entry.Status != MCPServerStatusOK || entry.LastError != "" {
		t.Fatalf("a success must clear the error: %+v", entry)
	}
}

// Unknown status values are refused (closed label set).
func TestSetMCPServerStatusClosedSet(t *testing.T) {
	gw := &NenyaGateway{}
	gw.SetMCPServerStatus("srv", "weird", nil)
	if _, ok := gw.MCPServerStatusSnapshot()["srv"]; ok {
		t.Fatal("an unknown status must be refused")
	}
}

type errTestFailure struct{}

func (errTestFailure) Error() string { return "boom" }

var _ error = errTestFailure{}
