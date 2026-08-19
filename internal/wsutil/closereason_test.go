package wsutil

import (
	"testing"
	"unicode/utf8"
)

func TestSafeCloseReasonRepairsShortInvalidUTF8(t *testing.T) {
	got := SafeCloseReason("upstream: \xffsecret")
	if !utf8.ValidString(got) {
		t.Fatalf("SafeCloseReason returned invalid UTF-8: %q", got)
	}
	if got != "upstream: ..." {
		t.Fatalf("SafeCloseReason = %q, want %q", got, "upstream: ...")
	}
}

func TestSafeCloseReasonPreservesValidAndTruncatesOnRuneBoundary(t *testing.T) {
	if got := SafeCloseReason("normal reason"); got != "normal reason" {
		t.Fatalf("valid short reason changed to %q", got)
	}
	input := "prefix-"
	for i := 0; i < 80; i++ {
		input += "界"
	}
	got := SafeCloseReason(input)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated reason is invalid UTF-8: %q", got)
	}
	if len(got) > MaxCloseReasonBytes {
		t.Fatalf("truncated reason has %d bytes, max %d", len(got), MaxCloseReasonBytes)
	}
	if got[len(got)-3:] != "..." {
		t.Fatalf("truncated reason missing suffix: %q", got)
	}
}
