// Package endpoint holds small helpers for ACP client endpoint path handling.
// Everything here is internal; if a
// helper needs to be consumed by SDK users, promote it into a public package.
package endpoint

import "strings"

// NormalizePath trims whitespace, ensures a leading '/', and strips any
// trailing '/'. An empty or "/"-only input returns "/". Server and proxy
// routes are owned by their host routers; this helper keeps the WS client's
// endpoint-path options consistent and prevents "looks-right but 404"
// mismatches.
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
