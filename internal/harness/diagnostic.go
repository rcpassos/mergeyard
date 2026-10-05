package harness

import "strings"

// Recovery recognizes native diagnostics for the exact requested conversation.
func matchesExactDiagnostic(data []byte, diagnostic string) bool {
	return strings.TrimSpace(string(data)) == diagnostic
}
