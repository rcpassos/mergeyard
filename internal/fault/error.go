// Package fault defines coded errors shared by configuration and runtime callers.
package fault

import "fmt"

// Error exposes a stable code, a human message, a field or filesystem path, and the underlying
// cause. Callers can inspect it with errors.As and the cause with errors.Is.
type Error struct {
	Code    string
	Message string
	Path    string
	Err     error
}

func (e *Error) Error() string {
	diagnostic := e.Code
	if e.Path != "" {
		diagnostic += ": " + e.Path
	}
	if e.Message != "" {
		diagnostic += ": " + e.Message
	}
	if e.Err != nil || e.Message == "" {
		diagnostic += fmt.Sprintf(": %v", e.Err)
	}
	return diagnostic
}

func (e *Error) Unwrap() error { return e.Err }
