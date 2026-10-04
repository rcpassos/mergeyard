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
	if e.Message != "" {
		if e.Path != "" {
			return fmt.Sprintf("%s: %s: %s", e.Code, e.Path, e.Message)
		}
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Path, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }
