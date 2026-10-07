---
name: implement
description: "Implement a piece of work based on a spec or set of tickets."
disable-model-invocation: true
---

Implement the work described by the user in the spec or tickets.

For ticket-required live observations, external access, or specific environments, read [implementation prerequisites](../../references/implementation-prerequisites.md) before implementation.

Before replacing a failed test run, read the owned-run cleanup guidance in that same reference.

Use /tdd where possible, at pre-agreed seams.

Run typechecking regularly, single test files regularly, and the full test suite once at the end.

Once done, use /code-review to review the work.

Commit your work to the current branch.
