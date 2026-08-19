package wsconn

import (
	"errors"
	"fmt"
	"testing"

	gorillawebsocket "github.com/gorilla/websocket"
	hertzwebsocket "github.com/hertz-contrib/websocket"
)

func TestFormatCloseMessage(t *testing.T) {
	payload := FormatCloseMessage(CloseGoingAway, "shutdown")
	want := []byte{0x03, 0xe9, 's', 'h', 'u', 't', 'd', 'o', 'w', 'n'}
	if string(payload) != string(want) {
		t.Fatalf("FormatCloseMessage() = %v, want %v", payload, want)
	}
	if payload := FormatCloseMessage(CloseNoStatusReceived, "ignored"); payload == nil || len(payload) != 0 {
		t.Fatalf("reserved close payload = %#v, want non-nil empty slice", payload)
	}
}

func TestCloseErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		text string
	}{
		{name: "normalized", err: &CloseError{Code: CloseGoingAway, Text: "bye"}, code: CloseGoingAway, text: "bye"},
		{name: "wrapped normalized", err: fmt.Errorf("read: %w", &CloseError{Code: CloseNormalClosure, Text: "done"}), code: CloseNormalClosure, text: "done"},
		{name: "hertz", err: &hertzwebsocket.CloseError{Code: ClosePolicyViolation, Text: "policy"}, code: ClosePolicyViolation, text: "policy"},
		{name: "wrapped gorilla", err: fmt.Errorf("read: %w", &gorillawebsocket.CloseError{Code: CloseMessageTooBig, Text: "large"}), code: CloseMessageTooBig, text: "large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := AsCloseError(tt.err)
			if !ok {
				t.Fatalf("AsCloseError(%T) did not recognize close error", tt.err)
			}
			if got.Code != tt.code || got.Text != tt.text {
				t.Fatalf("AsCloseError() = %#v, want code=%d text=%q", got, tt.code, tt.text)
			}
			if !IsCloseError(tt.err, tt.code) {
				t.Fatalf("IsCloseError(_, %d) = false", tt.code)
			}
			if IsUnexpectedCloseError(tt.err, tt.code) {
				t.Fatalf("IsUnexpectedCloseError(_, %d) = true", tt.code)
			}
			if !IsUnexpectedCloseError(tt.err, CloseInternalServerErr) {
				t.Fatal("IsUnexpectedCloseError() = false for an unexpected code")
			}
		})
	}

	if closeErr, ok := AsCloseError(errors.New("plain")); ok || closeErr != nil {
		t.Fatalf("AsCloseError(plain) = %#v, %v", closeErr, ok)
	}
}

func TestAdapterErrorNormalization(t *testing.T) {
	if !errors.Is(normalizeHertzError(hertzwebsocket.ErrReadLimit), ErrReadLimit) {
		t.Fatal("Hertz read-limit error was not normalized")
	}
	if !errors.Is(normalizeGorillaError(gorillawebsocket.ErrReadLimit), ErrReadLimit) {
		t.Fatal("Gorilla read-limit error was not normalized")
	}

	for name, err := range map[string]error{
		"hertz":   normalizeHertzError(&hertzwebsocket.CloseError{Code: CloseGoingAway, Text: "bye"}),
		"gorilla": normalizeGorillaError(&gorillawebsocket.CloseError{Code: CloseGoingAway, Text: "bye"}),
	} {
		t.Run(name, func(t *testing.T) {
			closeErr, ok := err.(*CloseError)
			if !ok || closeErr.Code != CloseGoingAway || closeErr.Text != "bye" {
				t.Fatalf("normalized error = %#v", err)
			}
		})
	}
}

func TestWrapNil(t *testing.T) {
	if got := WrapHertz(nil); got != nil {
		t.Fatalf("WrapHertz(nil) = %#v, want nil", got)
	}
	if got := WrapGorilla(nil); got != nil {
		t.Fatalf("WrapGorilla(nil) = %#v, want nil", got)
	}
}
