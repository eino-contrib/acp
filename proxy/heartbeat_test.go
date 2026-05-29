package proxy

import (
	"testing"
	"time"
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
