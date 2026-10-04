package sessions

import (
	"strings"
	"testing"
)

func TestNameIsSafeBoundedAndDistinguishesRunsAndAttempts(t *testing.T) {
	req := Request{RunID: strings.Repeat("run :.$' 界", 50), Phase: strings.Repeat("review :.$' 界", 50), Round: 1, Attempt: 1}
	first := Name(req)
	if len(first) > 96 || !strings.HasPrefix(first, "mergeyard-") || !strings.HasSuffix(first, "-1-1") {
		t.Fatalf("invalid name shape: %q", first)
	}
	for _, char := range first {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
			t.Fatalf("unsafe name: %q", first)
		}
	}
	if Name(req) != first {
		t.Fatal("name changes across restart")
	}
	req.RunID += "different suffix"
	if Name(req) == first {
		t.Fatal("runs with the same shortened prefix collide")
	}
	req.RunID = strings.Repeat("run :.$' 界", 50)
	req.Attempt++
	if Name(req) == first {
		t.Fatal("attempts collide")
	}
	req.Attempt = 1
	req.Round++
	if Name(req) == first {
		t.Fatal("rounds collide")
	}
}
