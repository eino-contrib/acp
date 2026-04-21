// Package wsutil holds small WebSocket helpers shared by the SDK's proxy and
// its example binaries. Everything here is internal; if a helper needs to be
// consumed by SDK users, promote it into a public package.
package wsutil

import "unicode/utf8"

// MaxCloseReasonBytes is the maximum number of bytes a WebSocket close frame's
// "reason" text may occupy. RFC 6455 caps control frames at 125-byte payloads,
// and 2 bytes are reserved for the close code.
const MaxCloseReasonBytes = 123

// SafeCloseReason truncates s so its UTF-8 encoding fits in MaxCloseReasonBytes
// without splitting a rune. When truncation occurs, the suffix "..." is
// appended and counted against the limit.
//
// Peers MUST validate close-frame reason text as UTF-8 (RFC 6455 §5.5.1) and
// otherwise treat the frame as a protocol error, so byte-level truncation on
// non-ASCII input (Chinese, emoji, ...) can silently corrupt the close
// handshake. Always route outbound reason strings through this helper.
func SafeCloseReason(s string) string {
	if len(s) <= MaxCloseReasonBytes {
		return s
	}
	const suffix = "..."
	limit := MaxCloseReasonBytes - len(suffix)
	end := 0
	for end < limit {
		r, size := utf8.DecodeRuneInString(s[end:])
		if size == 0 {
			break
		}
		// Stop before an invalid byte rather than emit a replacement char; the
		// caller's input should be valid UTF-8, but defensive handling keeps
		// the output well-formed even if an upstream string was damaged.
		if r == utf8.RuneError && size == 1 {
			break
		}
		if end+size > limit {
			break
		}
		end += size
	}
	return s[:end] + suffix
}
