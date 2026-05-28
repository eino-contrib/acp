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

	t.Run("DeprecatedWithWebSocketPongTimeout_maps_to_wsReadTimeout", func(t *testing.T) {
		opts := defaultOptions()
		WithWebSocketPongTimeout(60 * time.Second)(&opts)
		if opts.wsReadTimeout != 60*time.Second {
			t.Fatalf("expected wsReadTimeout=60s via deprecated PongTimeout, got %v", opts.wsReadTimeout)
		}
		if opts.wsPongTimeout != 60*time.Second {
			t.Fatalf("expected wsPongTimeout=60s, got %v", opts.wsPongTimeout)
		}
	})

	t.Run("DeprecatedWithWebSocketPingInterval_accepted", func(t *testing.T) {
		opts := defaultOptions()
		WithWebSocketPingInterval(45 * time.Second)(&opts)
		if opts.wsPingInterval != 45*time.Second {
			t.Fatalf("expected wsPingInterval=45s, got %v", opts.wsPingInterval)
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

func TestProxyDeprecatedOptionCompatibility(t *testing.T) {
	// Verify deprecated options compile, apply without panic, and do not
	// interfere with the new options when used together.
	opts := defaultOptions()

	WithWebSocketPingInterval(30 * time.Second)(&opts)
	WithWebSocketPongTimeout(75 * time.Second)(&opts)
	WithWebSocketReadTimeout(120 * time.Second)(&opts)
	WithWebSocketFirstFrameTimeout(20 * time.Second)(&opts)

	// ReadTimeout should reflect the last explicit set, not the deprecated mapping
	if opts.wsReadTimeout != 120*time.Second {
		t.Fatalf("expected wsReadTimeout=120s (explicit wins over deprecated), got %v", opts.wsReadTimeout)
	}
	if opts.firstFrameTimeout != 20*time.Second {
		t.Fatalf("expected firstFrameTimeout=20s, got %v", opts.firstFrameTimeout)
	}
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

	// Deprecated fields should be zero in defaults
	if opts.wsPingInterval != 0 {
		t.Fatalf("default wsPingInterval should be 0, got %v", opts.wsPingInterval)
	}
	if opts.wsPongTimeout != 0 {
		t.Fatalf("default wsPongTimeout should be 0, got %v", opts.wsPongTimeout)
	}
}
