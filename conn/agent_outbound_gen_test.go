package conn

import (
	"reflect"
	"testing"

	"github.com/eino-contrib/acp/internal/connspi"
)

// TestAgentOutboundParamsImplementSessionIDProvider dynamically walks every
// exported method on *AgentConnection and asserts that any params type coming
// from the acp package (i.e. generated from schema.json, as in
// agent_outbound_gen.go) and carrying a top-level SessionID field implements
// connspi.SessionIDProvider.
//
// No hardcoded list — adding a new outbound method via cmd/generate is
// automatically covered.
func TestAgentOutboundParamsImplementSessionIDProvider(t *testing.T) {
	const acpPkg = "github.com/eino-contrib/acp"

	providerType := reflect.TypeOf((*connspi.SessionIDProvider)(nil)).Elem()
	agentType := reflect.TypeOf((*AgentConnection)(nil))

	checked := 0
	for i := 0; i < agentType.NumMethod(); i++ {
		m := agentType.Method(i)
		// Method signature: func(recv, ctx, params) ...
		if m.Type.NumIn() < 3 {
			continue
		}
		paramType := m.Type.In(2)
		if paramType.Kind() != reflect.Struct || paramType.PkgPath() != acpPkg {
			continue
		}
		if _, ok := paramType.FieldByName("SessionID"); !ok {
			continue
		}
		checked++
		if !paramType.Implements(providerType) && !reflect.PointerTo(paramType).Implements(providerType) {
			t.Errorf("AgentConnection.%s params %s does not implement connspi.SessionIDProvider",
				m.Name, paramType)
		}
	}

	if checked == 0 {
		t.Fatal("no acp.* params types discovered on *AgentConnection; reflection walk is broken")
	}
	t.Logf("verified %d outbound params types implement SessionIDProvider", checked)
}
