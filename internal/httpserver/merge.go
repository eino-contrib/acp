package httpserver

import "github.com/eino-contrib/acp/internal/safe"

// MergeDone returns a channel that closes when any of the input channels close.
//
// The caller must ensure that at least one of the input channels eventually
// closes; otherwise the internal goroutine will leak.
func MergeDone(channels ...<-chan struct{}) <-chan struct{} {
	return safe.MergeDone(channels...)
}
