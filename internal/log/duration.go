package log

import "time"

// ValidateDuration checks a duration value used by WebSocket Option functions.
// It returns true if the value should be applied.
//
//   - Negative values: warn and return false (caller should skip assignment).
//   - Positive values below minPositive: warn but return true (caller applies).
//   - All other values: return true silently.
//
// role identifies the component (e.g. "client", "server", "proxy").
// option is the Option function name (e.g. "WithPingInterval").
func ValidateDuration(role, option string, d time.Duration, minPositive time.Duration) bool {
	if d < 0 {
		Warn("[ws] role=%s option=%s value=%v constraint=\"must be >= 0\" action=ignored", role, option, d)
		return false
	}
	if d > 0 && d < minPositive {
		Warn("[ws] role=%s option=%s value=%v constraint=\"production value should be >= %v\"", role, option, d, minPositive)
	}
	return true
}
