package harness

import "time"

// FailureClassification describes account availability, separately from the
// task's structured result. Only completed unsuccessful native executions may
// be classified. Unknown diagnostics must remain Ordinary.
type FailureKind string

const (
	Ordinary         FailureKind = "ordinary"
	TemporaryLimit   FailureKind = "temporary_limit"
	CreditsExhausted FailureKind = "credits_exhausted"
)

type FailureClassification struct {
	Kind FailureKind `json:"kind"`
	// ResetAt is zero unless the adapter has established a reliable future reset.
	ResetAt time.Time `json:"reset_at,omitempty"`
	Source  string    `json:"source,omitempty"`
}
