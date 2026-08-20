package server

import (
	"context"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
)

// TestWebSocketOptionStorage verifies that public WithWebSocket* options
// on ACPServer correctly store values into the server struct fields.
func TestWebSocketOptionStorage(t *testing.T) {
	factory := func(ctx context.Context) acp.Agent {
		return &acp.BaseAgent{}
	}

	readTimeout := 90 * time.Second
	initTimeout := 20 * time.Second

	srv, err := NewACPServer(factory,
		WithWebSocketReadTimeout(readTimeout),
		WithWebSocketInitializeTimeout(initTimeout),
	)
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}
	defer srv.Close()

	if srv.wsReadTimeout != readTimeout {
		t.Errorf("wsReadTimeout = %v, want %v", srv.wsReadTimeout, readTimeout)
	}
	if srv.wsInitializeTimeout != initTimeout {
		t.Errorf("wsInitializeTimeout = %v, want %v", srv.wsInitializeTimeout, initTimeout)
	}
}

func TestWebSocketOptionDefaults(t *testing.T) {
	factory := func(ctx context.Context) acp.Agent {
		return &acp.BaseAgent{}
	}

	srv, err := NewACPServer(factory)
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}
	defer srv.Close()

	if srv.wsReadTimeout != 0 {
		t.Errorf("default wsReadTimeout = %v, want 0", srv.wsReadTimeout)
	}
	if srv.wsInitializeTimeout != 15*time.Second {
		t.Errorf("default wsInitializeTimeout = %v, want 15s", srv.wsInitializeTimeout)
	}
}

func TestNewACPServerIgnoresNilOption(t *testing.T) {
	srv, err := NewACPServer(func(context.Context) acp.Agent { return &acp.BaseAgent{} }, nil)
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}
	defer srv.Close()
}

func TestWebSocketOptionNegativeIgnored(t *testing.T) {
	factory := func(ctx context.Context) acp.Agent {
		return &acp.BaseAgent{}
	}

	srv, err := NewACPServer(factory,
		WithWebSocketReadTimeout(-1*time.Second),
		WithWebSocketInitializeTimeout(-5*time.Second),
	)
	if err != nil {
		t.Fatalf("NewACPServer: %v", err)
	}
	defer srv.Close()

	// Negative values should be ignored, leaving defaults.
	if srv.wsReadTimeout != 0 {
		t.Errorf("wsReadTimeout = %v, want 0 (negative ignored)", srv.wsReadTimeout)
	}
	if srv.wsInitializeTimeout != 15*time.Second {
		t.Errorf("wsInitializeTimeout = %v, want 15s (negative ignored)", srv.wsInitializeTimeout)
	}
}
