// Package endpoint holds small helpers for ACP endpoint path handling shared
// by server, proxy, and ws transports. Everything here is internal; if a
// helper needs to be consumed by SDK users, promote it into a public package.
package endpoint

import "strings"

// NormalizePath trims whitespace, ensures a leading '/', and strips any
// trailing '/'. An empty or "/"-only input returns "/". This keeps endpoint
// path semantics identical across server.WithEndpoint, proxy.WithEndpoint,
// and the WS client transport so operators cannot hit "looks-right but 404"
// routing mismatches.
func NormalizePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed == "/" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}
