package testkit

import (
	"time"

	"github.com/rcpassos/mergeyard/internal/harness"
)

// Fake adapters return normalized outcomes; native signals belong to #81/#82.
type ClassifiedHarness struct {
	BeforeClassify func()
	harness.HarnessAdapter
	Outcome  harness.FailureClassification
	Calls    int
	Phases   []harness.PhaseContext
	Outcomes []harness.FailureClassification
	Observed []time.Time
}

func (f *ClassifiedHarness) ClassifyFailure(a harness.PhaseArtifacts, observed time.Time) harness.FailureClassification {
	if f.BeforeClassify != nil {
		f.BeforeClassify()
	}
	f.Calls++
	f.Observed = append(f.Observed, observed)
	if f.Calls <= len(f.Outcomes) {
		return f.Outcomes[f.Calls-1]
	}
	return f.Outcome
}
func (f *ClassifiedHarness) BuildInvocation(p harness.PhaseContext, r harness.RoleConfig) (harness.PhaseInvocation, error) {
	f.Phases = append(f.Phases, p)
	return f.HarnessAdapter.BuildInvocation(p, r)
}
func (f *ClassifiedHarness) DiscoverSession(data []byte) (string, error) {
	if d, ok := f.HarnessAdapter.(harness.SessionDiscoverer); ok {
		return d.DiscoverSession(data)
	}
	return "", nil
}

func CodexStructuredResult(result string) string {
	return `last_message=''
previous=''
for arg do
 if [ "$previous" = '-o' ]; then last_message=$arg; fi
 previous=$arg
done
printf '%s\n' '` + result + `' > "$last_message"
printf '%s\n' '{"type":"turn.completed"}'`
}

func (f *ClassifiedHarness) CheckConfigurationInvocation(command harness.Invocation) harness.Invocation {
	if adapter, ok := f.HarnessAdapter.(harness.CheckConfigurator); ok {
		return adapter.CheckConfigurationInvocation(command)
	}
	return harness.Invocation{}
}
func (f *ClassifiedHarness) ConfigureCheckInvocation(command harness.Invocation, data []byte) (harness.Invocation, error) {
	return f.HarnessAdapter.(harness.CheckConfigurator).ConfigureCheckInvocation(command, data)
}
