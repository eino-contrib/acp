// Package methodmeta holds generated ACP method capability metadata.
//
// It is an internal package so that the exact metadata shape can evolve
// without breaking external callers. SDK packages (acp root, conn tests)
// consume it through the helpers in this file.
package methodmeta

// Side describes whether a wire method is implemented by the agent or the
// client side of the ACP connection.
type Side string

const (
	SideAgent  Side = "agent"
	SideClient Side = "client"
	SideBoth   Side = "both"
)

// Metadata describes generated capabilities for a single ACP wire method.
type Metadata struct {
	Key        string
	WireMethod string
	Side       Side
	// SupportsRequest and SupportsNotification describe which JSON-RPC forms a
	// wire method accepts. Most methods are one or the other; an overloaded
	// method (e.g. mcp/message) sets both, meaning it is dispatched as a request
	// when a JSON-RPC id is present and as a notification when it is absent.
	SupportsRequest       bool
	SupportsNotification  bool
	SessionBound          bool
	SessionHeaderRequired bool
	SessionCreating       bool
}

// Lookup returns generated metadata for the given ACP wire method.
func Lookup(method string) (Metadata, bool) {
	meta, ok := methodMetadata[method]
	return meta, ok
}

// All returns a copy of every registered method metadata entry. Useful for
// tests that verify handler coverage.
func All() []Metadata {
	result := make([]Metadata, 0, len(methodMetadata))
	for _, meta := range methodMetadata {
		result = append(result, meta)
	}
	return result
}
