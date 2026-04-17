package safe

import (
	"runtime/debug"

	"github.com/eino-contrib/acp/internal/log"
)

func GoWithLogger(logger log.Logger, f func()) {
	if logger == nil {
		logger = log.Default()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("[GoWithLogger] panic: %v\n%s", r, debug.Stack())
			}
		}()
		f()
	}()
}

// CancelOnDone spawns a goroutine that calls cancel when any of the provided
// done channels close. This eliminates the repeated pattern of:
//
//	go func() { select { case <-a: cancel() case <-b: cancel() case <-ctx.Done(): } }()
//
// The goroutine exits when cancel is called (ctx.Done closes) or when any done
// channel fires.
func CancelOnDone(logger log.Logger, cancel func(), ctxDone <-chan struct{}, doneChannels ...<-chan struct{}) {
	// Filter nil channels.
	filtered := make([]<-chan struct{}, 0, len(doneChannels))
	for _, ch := range doneChannels {
		if ch != nil {
			filtered = append(filtered, ch)
		}
	}

	GoWithLogger(logger, func() {
		switch len(filtered) {
		case 0:
			<-ctxDone
		case 1:
			select {
			case <-ctxDone:
			case <-filtered[0]:
			}
		case 2:
			select {
			case <-ctxDone:
			case <-filtered[0]:
			case <-filtered[1]:
			}
		case 3:
			select {
			case <-ctxDone:
			case <-filtered[0]:
			case <-filtered[1]:
			case <-filtered[2]:
			}
		default:
			// For >3 extra channels (extremely rare in practice), merge them
			// pairwise and select on the merged result + ctxDone.
			merged := MergeDone(filtered...)
			select {
			case <-ctxDone:
			case <-merged:
			}
		}
		cancel()
	})
}

// MergeDone returns a channel that closes when any of the input channels close.
//
// The caller must ensure that at least one of the input channels eventually
// closes; otherwise the internal goroutine will leak.
func MergeDone(channels ...<-chan struct{}) <-chan struct{} {
	filtered := make([]<-chan struct{}, 0, len(channels))
	for _, ch := range channels {
		if ch != nil {
			filtered = append(filtered, ch)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	case 2:
		merged := make(chan struct{})
		go func() {
			select {
			case <-filtered[0]:
			case <-filtered[1]:
			}
			close(merged)
		}()
		return merged
	case 3:
		merged := make(chan struct{})
		go func() {
			select {
			case <-filtered[0]:
			case <-filtered[1]:
			case <-filtered[2]:
			}
			close(merged)
		}()
		return merged
	default:
		// For >3 channels (rare), chain pairwise merges.
		result := MergeDone(filtered[0], filtered[1])
		for i := 2; i < len(filtered); i++ {
			result = MergeDone(result, filtered[i])
		}
		return result
	}
}
