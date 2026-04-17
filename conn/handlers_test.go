package conn

import (
	"sort"
	"testing"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/methodmeta"
)

func TestAgentSideConnectionRegistersAllGeneratedMethods(t *testing.T) {
	t.Parallel()

	conn := NewAgentConnectionFromTransport(acp.BaseAgent{}, newChannelTransport())
	assertHandlerCoverage(t, "agent request", handlerKeys(conn.requestHandlers), expectedMethodKeys(methodmeta.SideAgent, false))
	assertHandlerCoverage(t, "agent notification", handlerKeys(conn.notificationHandlers), expectedMethodKeys(methodmeta.SideAgent, true))
}

func TestClientConnectionRegistersAllGeneratedMethods(t *testing.T) {
	t.Parallel()

	conn := NewClientConnection(acp.BaseClient{}, newChannelTransport())
	assertHandlerCoverage(t, "client request", handlerKeys(conn.requestHandlers), expectedMethodKeys(methodmeta.SideClient, false))
	assertHandlerCoverage(t, "client notification", handlerKeys(conn.notificationHandlers), expectedMethodKeys(methodmeta.SideClient, true))
}

func expectedMethodKeys(side methodmeta.Side, notification bool) []string {
	keys := make([]string, 0)
	for _, meta := range methodmeta.All() {
		if meta.Side == side && meta.Notification == notification {
			keys = append(keys, meta.WireMethod)
		}
	}
	sort.Strings(keys)
	return keys
}

func handlerKeys[T any](handlers map[string]T) []string {
	keys := make([]string, 0, len(handlers))
	for method := range handlers {
		keys = append(keys, method)
	}
	sort.Strings(keys)
	return keys
}

func assertHandlerCoverage(t *testing.T, label string, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s handler count = %d, want %d\n got=%v\nwant=%v", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s handlers mismatch\n got=%v\nwant=%v", label, got, want)
		}
	}
}
