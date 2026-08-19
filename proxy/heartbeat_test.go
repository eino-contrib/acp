package proxy

import (
	"fmt"
	"testing"
	"time"

	"github.com/eino-contrib/acp/internal/wsconn"
)

func TestProxyOptions(t *testing.T) {
	t.Run("WithWebSocketReadTimeout", func(t *testing.T) {
		opts := defaultOptions()
		WithWebSocketReadTimeout(90 * time.Second)(&opts)
		if opts.wsReadTimeout != 90*time.Second {
			t.Fatalf("expected wsReadTimeout=90s, got %v", opts.wsReadTimeout)
		}
	})

	t.Run("WithWebSocketFirstFrameTimeout", func(t *testing.T) {
		opts := defaultOptions()
		WithWebSocketFirstFrameTimeout(10 * time.Second)(&opts)
		if opts.firstFrameTimeout != 10*time.Second {
			t.Fatalf("expected firstFrameTimeout=10s, got %v", opts.firstFrameTimeout)
		}
	})

	t.Run("zero_value_disables_timeout", func(t *testing.T) {
		opts := defaultOptions()
		WithWebSocketReadTimeout(0)(&opts)
		if opts.wsReadTimeout != 0 {
			t.Fatalf("expected wsReadTimeout=0 (disabled), got %v", opts.wsReadTimeout)
		}
		WithWebSocketFirstFrameTimeout(0)(&opts)
		if opts.firstFrameTimeout != 0 {
			t.Fatalf("expected firstFrameTimeout=0 (disabled), got %v", opts.firstFrameTimeout)
		}
	})

	t.Run("negative_value_ignored", func(t *testing.T) {
		opts := defaultOptions()
		original := opts.wsReadTimeout
		WithWebSocketReadTimeout(-1 * time.Second)(&opts)
		if opts.wsReadTimeout != original {
			t.Fatalf("negative value should be ignored, got %v", opts.wsReadTimeout)
		}
	})

	// Deprecated options retained for source-compatibility with the
	// pre-heartbeat-refactor Proxy API. These tests pin down the documented
	// migration contract: WithWebSocketPingInterval is a no-op, and
	// WithWebSocketPongTimeout aliases WithWebSocketReadTimeout.
	t.Run("WithWebSocketPingInterval_is_noop", func(t *testing.T) {
		opts := defaultOptions()
		// Snapshot the timeout-related fields the compatibility option could
		// plausibly touch.
		beforeRead := opts.wsReadTimeout
		beforeWrite := opts.wsWriteTimeout
		beforeFirst := opts.firstFrameTimeout
		beforeHandshake := opts.handshakeTimeout
		WithWebSocketPingInterval(30 * time.Second)(&opts)
		if opts.wsReadTimeout != beforeRead ||
			opts.wsWriteTimeout != beforeWrite ||
			opts.firstFrameTimeout != beforeFirst ||
			opts.handshakeTimeout != beforeHandshake {
			t.Fatalf("WithWebSocketPingInterval should be a no-op, timeouts changed")
		}
	})

	t.Run("WithWebSocketPongTimeout_maps_to_ReadTimeout", func(t *testing.T) {
		opts := defaultOptions()
		WithWebSocketPongTimeout(75 * time.Second)(&opts)
		if opts.wsReadTimeout != 75*time.Second {
			t.Fatalf("WithWebSocketPongTimeout should set wsReadTimeout=75s, got %v", opts.wsReadTimeout)
		}
	})

	t.Run("WithWebSocketPongTimeout_negative_ignored", func(t *testing.T) {
		opts := defaultOptions()
		original := opts.wsReadTimeout
		WithWebSocketPongTimeout(-1 * time.Second)(&opts)
		if opts.wsReadTimeout != original {
			t.Fatalf("negative WithWebSocketPongTimeout should be ignored, got %v", opts.wsReadTimeout)
		}
	})
}

func TestProxyDefaultOptions(t *testing.T) {
	opts := defaultOptions()

	if opts.wsReadTimeout != DefaultWebSocketReadTimeout {
		t.Fatalf("default wsReadTimeout: want %v, got %v", DefaultWebSocketReadTimeout, opts.wsReadTimeout)
	}
	if opts.firstFrameTimeout != DefaultWebSocketFirstFrameTimeout {
		t.Fatalf("default firstFrameTimeout: want %v, got %v", DefaultWebSocketFirstFrameTimeout, opts.firstFrameTimeout)
	}
	if opts.wsWriteTimeout != DefaultWebSocketWriteTimeout {
		t.Fatalf("default wsWriteTimeout: want %v, got %v", DefaultWebSocketWriteTimeout, opts.wsWriteTimeout)
	}
	if opts.maxMessageSize != DefaultMaxMessageSize {
		t.Fatalf("default maxMessageSize: want %v, got %v", DefaultMaxMessageSize, opts.maxMessageSize)
	}
	if opts.handshakeTimeout != DefaultHandshakeTimeout {
		t.Fatalf("default handshakeTimeout: want %v, got %v", DefaultHandshakeTimeout, opts.handshakeTimeout)
	}
	if opts.maxConcurrent != DefaultMaxConcurrentConnections {
		t.Fatalf("default maxConcurrent: want %v, got %v", DefaultMaxConcurrentConnections, opts.maxConcurrent)
	}
}

func TestClassifyReadLimitDoesNotDuplicateLibraryCloseFrame(t *testing.T) {
	action := classifyPumpErr(fmt.Errorf("read: %w", wsconn.ErrReadLimit))
	if action.SendFrame {
		t.Fatalf("read-limit action requested a duplicate close frame: %#v", action)
	}
	if action.Reason != "message too big" {
		t.Fatalf("reason = %q, want %q", action.Reason, "message too big")
	}

	downstream := classifyPumpErr(fmt.Errorf("downstream: %w", errPayloadTooLarge))
	if !downstream.SendFrame || downstream.Code != wsconn.CloseMessageTooBig {
		t.Fatalf("downstream oversize action = %#v, want one 1009 frame", downstream)
	}
}
