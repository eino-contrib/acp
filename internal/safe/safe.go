package safe

import (
	"runtime/debug"

	"github.com/eino-contrib/acp/internal/log"
)

// maxPanicStackBytes bounds the stack trace length written to logs so a
// runaway panic cannot flood the logger. 8KB is enough for deep real-world
// Go stacks and still cheap to emit.
const maxPanicStackBytes = 8 * 1024

// TruncatedStack returns debug.Stack() capped at maxPanicStackBytes with a
// suffix marker when truncation occurred. Exported so other panic-recovery
// sites (HTTP direct dispatch, jsonrpc connection) can share the same bound
// instead of calling debug.Stack() directly and risking log/memory floods
// during panic storms.
func TruncatedStack() []byte {
	return truncatedStack()
}

// truncatedStack returns debug.Stack() capped at maxPanicStackBytes with a
// suffix marker when truncation occurred.
func truncatedStack() []byte {
	stack := debug.Stack()
	if len(stack) <= maxPanicStackBytes {
		return stack
	}
	truncated := make([]byte, 0, maxPanicStackBytes+len("\n... [truncated]"))
	truncated = append(truncated, stack[:maxPanicStackBytes]...)
	truncated = append(truncated, "\n... [truncated]"...)
	return truncated
}

func Go(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("[safe.Go] panic: %v\n%s", r, truncatedStack())
			}
		}()
		f()
	}()
}

// GoRecover runs f in a goroutine with panic recovery. When f panics, the
// recovered value is logged and onPanic is invoked with it. Use this in place
// of Go for goroutines whose silent death would leave waiters blocked (e.g.
// SSE consumers whose pending request must receive a synthetic error so the
// caller does not hang forever).
//
// onPanic runs inside the same deferred recover, so it must not itself panic.
// onPanic may be nil, in which case behavior degrades to Go.
func GoRecover(f func(), onPanic func(recovered any)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("[safe.GoRecover] panic: %v\n%s", r, truncatedStack())
				if onPanic != nil {
					defer func() {
						if rr := recover(); rr != nil {
							log.Error("[safe.GoRecover] onPanic panic: %v", rr)
						}
					}()
					onPanic(r)
				}
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
func CancelOnDone(cancel func(), ctxDone <-chan struct{}, doneChannels ...<-chan struct{}) {
	filtered := make([]<-chan struct{}, 0, len(doneChannels))
	for _, ch := range doneChannels {
		if ch != nil {
			filtered = append(filtered, ch)
		}
	}

	Go(func() {
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
		result := MergeDone(filtered[0], filtered[1])
		for i := 2; i < len(filtered); i++ {
			result = MergeDone(result, filtered[i])
		}
		return result
	}
}
