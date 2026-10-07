# Implementation prerequisites

## Ticket-required external conditions

Apply this check when an acceptance criterion requires a real observation,
external access, or a specific environment. Code-only tickets without these
requirements proceed without additional probes.

Before implementation, identify the required condition and the evidence that
would establish it. Inspect relevant existing evidence first, checking that its
version, environment, identity and outcome match the criterion. Distinguish
captured observations from source expectations, synthetic fixtures and fallback
assumptions. A successful request cannot establish a required failure.

When a live probe is needed, use the authorization already provided by the ticket
or session. State its invocation and time bounds before running it. Ask only for
missing authorization needed for that action; elapsed time is not authorization.
Keep requests bounded and avoid repeated probing or manufacturing the condition
through unrelated work.

If the required condition is unavailable or remains unverified, stop the affected
ticket before implementation or preparation tooling. Report the unmet acceptance
criterion, the observed evidence, and the next capture step or condition needed
to resume. Leave the criterion unresolved. Preparation tooling can proceed as
explicitly scoped separate work; its completion does not satisfy the original
live-evidence requirement.

For a spec with several tickets, keep externally blocked tickets and their
dependents out of the executable frontier. Continue unrelated unblocked tickets.
Supply the evidence and this reference to implementers so they check the condition
before starting. Retain blockers in the final report; an unresolved criterion
prevents claiming the ticket or entire spec complete, closing it, or declaring
its release gate passed.

## Replacing a failed test run

Before starting a replacement, confirm that the failed run has exited. If it is
still running, stop only the process group, wrapper or session owned by that
invocation and verify exit before retrying. Use recorded execution identities and
available process-management tools. Preserve output needed to explain the failure.
An unknown process or ambiguous ownership requires inspection rather than a broad
process-name kill. Reuse existing authorization when it covers the replacement.
