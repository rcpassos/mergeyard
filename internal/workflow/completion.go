package workflow

import (
	"errors"

	"github.com/rcpassos/mergeyard/internal/fault"
)

// ValidCompletion reports whether a finished attempt produced a publishable
// report: a normal success or a valid blocked/failed task result. Infrastructure
// failures are not publishable. Callers freeze this answer when the report is
// saved, so a later lifecycle rejection of the same attempt never withdraws it.
func ValidCompletion(err error) bool {
	if err == nil {
		return true
	}
	var coded *fault.Error
	if !errors.As(err, &coded) {
		return false
	}
	return coded.Code == "phase.blocked" || coded.Code == "phase.failed"
}
