package relay

import (
	"strings"
	"testing"
)

func TestContentAuditListHas(t *testing.T) {
	t.Setenv("CONTENT_AUDIT_TOKEN_IDS", "1, 10397,2")
	if !contentAuditListHas("CONTENT_AUDIT_TOKEN_IDS", "10397") {
		t.Fatal("expected token match")
	}
	if contentAuditListHas("CONTENT_AUDIT_TOKEN_IDS", "1039") {
		t.Fatal("unexpected partial token match")
	}
}

func TestContentAuditBytesTruncates(t *testing.T) {
	t.Setenv("CONTENT_AUDIT_MAX_BODY_BYTES", "4")
	body, truncated := contentAuditBytes([]byte("abcdef"))
	if body != "abcd" || !truncated {
		t.Fatalf("body=%q truncated=%v", body, truncated)
	}
	body, truncated = contentAuditBytes([]byte(strings.Repeat("x", 3)))
	if body != "xxx" || truncated {
		t.Fatalf("body=%q truncated=%v", body, truncated)
	}
}
