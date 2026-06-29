package main

import (
	"fmt"
	"sort"
	"strings"
)

// MethodInfo holds parsed information about an RPC method.
type MethodInfo struct {
	Key         string // meta key, e.g. "fs_read_text_file"
	WireMethod  string // wire name, e.g. "fs/read_text_file"
	Side        string // "agent" or "client"
	GoName      string // Go method name, e.g. "ReadTextFile"
	ReqType     string // Go request type, e.g. "ReadTextFileRequest"
	RespType    string // Go response type, e.g. "ReadTextFileResponse" (empty for notifications)
	Description string // from schema description
	IsNotify    bool   // true if notification (no response)
	IsUnstable  bool   // true if the schema marks the method as unstable
}

// GenerateInterfaces produces client_gen.go and agent_gen.go source code.
func (g *Generator) GenerateInterfaces(pkg string) (clientSrc []byte, agentSrc []byte, err error) {
	clientMethods := g.buildMethods(g.meta.ClientMethods, "client")
	agentMethods := g.buildMethods(g.meta.AgentMethods, "agent")

	clientSrc, err = g.renderInterfaceFile(pkg, "Client", clientMethods)
	if err != nil {
		return nil, nil, fmt.Errorf("client interface: %w", err)
	}

	agentSrc, err = g.renderInterfaceFile(pkg, "Agent", agentMethods)
	if err != nil {
		return nil, nil, fmt.Errorf("agent interface: %w", err)
	}

	return clientSrc, agentSrc, nil
}

func (g *Generator) buildMethods(methods map[string]string, side string) []MethodInfo {
	var result []MethodInfo

	for key, wireMethod := range methods {
		// Find request/response/notification types by matching x-method and x-side.
		reqName, respName, notifName := g.findTypesForMethod(wireMethod, side)

		// A wire method may support both a request and a notification form (the
		// only schema example is mcp/message). Emit a distinct MethodInfo for
		// each form so both an inbound request (JSON-RPC id present) and an
		// inbound notification (id absent) have a generated method + handler.
		// Without this, an overloaded method collapses to notification-only and
		// its request dispatches to method-not-found.
		overloaded := reqName != "" && notifName != ""

		if reqName != "" {
			mi := MethodInfo{Key: key, WireMethod: wireMethod, Side: side}
			mi.ReqType = toTitleCase(reqName)
			if respName != "" {
				mi.RespType = toTitleCase(respName)
			}
			mi.Description = g.getDescription(reqName)
			if overloaded {
				// Derive the base name from the meta key (mcp_message →
				// MCPMessage) rather than the request type (MessageMcpRequest →
				// MessageMCP), so the request keeps the canonical wire name.
				mi.GoName = toTitleCase(key)
			} else {
				// Derive method name from request type: ReadTextFileRequest → ReadTextFile
				mi.GoName = strings.TrimSuffix(toTitleCase(reqName), "Request")
			}
			result = append(result, g.finalizeMethod(mi))
		}

		if notifName != "" {
			mi := MethodInfo{Key: key, WireMethod: wireMethod, Side: side}
			mi.ReqType = toTitleCase(notifName)
			mi.IsNotify = true
			mi.Description = g.getDescription(notifName)
			// Derive method name from the meta key (SessionNotification →
			// SessionUpdate). When the method is overloaded, suffix the
			// notification form so it does not collide with the request method.
			mi.GoName = toTitleCase(key)
			if overloaded {
				mi.GoName += "Notification"
			}
			result = append(result, g.finalizeMethod(mi))
		}

		if reqName == "" && notifName == "" {
			mi := MethodInfo{Key: key, WireMethod: wireMethod, Side: side}
			mi.GoName = toTitleCase(key)
			result = append(result, g.finalizeMethod(mi))
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].WireMethod != result[j].WireMethod {
			return result[i].WireMethod < result[j].WireMethod
		}
		// Tiebreaker for overloaded wire methods so output is deterministic.
		return result[i].GoName < result[j].GoName
	})

	return result
}

// finalizeMethod applies the unstable-prefix rule shared by every method form.
func (g *Generator) finalizeMethod(mi MethodInfo) MethodInfo {
	mi.IsUnstable = isUnstableDescription(mi.Description)
	if mi.IsUnstable && !strings.HasPrefix(mi.GoName, "Unstable") {
		mi.GoName = "Unstable" + mi.GoName
	}
	return mi
}

func (g *Generator) findTypesForMethod(wireMethod, side string) (reqName, respName, notifName string) {
	for name, schema := range g.schema.Defs {
		xMethod, _ := schema.XMethod()
		xSide, _ := schema.XSide()
		// "both" types (e.g. mcp/message) belong to both client and agent sides.
		if xMethod != wireMethod || (xSide != side && xSide != "both") {
			continue
		}

		if strings.HasSuffix(name, "Notification") {
			notifName = name
		} else if strings.HasSuffix(name, "Request") {
			reqName = name
		} else if strings.HasSuffix(name, "Response") {
			respName = name
		}
	}
	return
}

func (g *Generator) getDescription(defName string) string {
	if s, ok := g.schema.Defs[defName]; ok && s.Description != nil {
		return strings.TrimSpace(*s.Description)
	}
	return ""
}

func isUnstableDescription(desc string) bool {
	desc = strings.TrimSpace(desc)
	return strings.HasPrefix(desc, "**UNSTABLE**")
}

func (g *Generator) renderInterfaceFile(pkg, ifaceName string, methods []MethodInfo) ([]byte, error) {
	var buf strings.Builder

	buf.WriteString("// Code generated by cmd/generate; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", pkg)
	buf.WriteString("import \"context\"\n\n")

	// Interface
	fmt.Fprintf(&buf, "// %s defines the %s-side RPC interface.\n", ifaceName, strings.ToLower(ifaceName))
	fmt.Fprintf(&buf, "type %s interface {\n", ifaceName)

	for _, m := range methods {
		if m.Description != "" {
			for _, line := range strings.Split(m.Description, "\n") {
				fmt.Fprintf(&buf, "\t// %s\n", strings.TrimSpace(line))
			}
		}
		if m.IsNotify {
			fmt.Fprintf(&buf, "\t%s(ctx context.Context, params %s) error\n", m.GoName, m.ReqType)
		} else {
			fmt.Fprintf(&buf, "\t%s(ctx context.Context, params %s) (%s, error)\n", m.GoName, m.ReqType, m.RespType)
		}
	}

	buf.WriteString("}\n\n")

	// Method name constants
	fmt.Fprintf(&buf, "// %s method wire names.\n", ifaceName)
	fmt.Fprintf(&buf, "const (\n")
	for _, m := range methods {
		fmt.Fprintf(&buf, "\tMethod%s%s = %q\n", ifaceName, m.GoName, m.WireMethod)
	}
	fmt.Fprintf(&buf, ")\n")

	return formatSource(buf.String())
}
