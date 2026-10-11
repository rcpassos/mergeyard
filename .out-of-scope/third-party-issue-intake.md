# Third-party issue intake

Mergeyard does not create issues from external sources such as Sentry, PostHog or other error trackers and analytics tools.

## Why this is out of scope

Mergeyard turns approved issues into reviewed, merge-ready pull requests. GitHub owns the work (PRD §4.2): a human writes or approves an issue, and the ready label is the explicit authorization to start it. An intake command would make Mergeyard an issue creator as well as an issue consumer, and that blurs the boundary.

Each source also brings its own API credentials, SDK or HTTP client, pagination, rate limits and deduplication rules. That works against "simple by default" (PRD §4.1). It also widens the set of secrets the control plane has to handle, which is the opposite of the credential isolation pursued in #103.

A companion tool fits this better. It can follow the same GitHub conventions: issues created without the ready label and labelled `needs-triage`, so Mergeyard only sees them after a human approves them. Mergeyard needs no changes to work with such a tool.

## Not covered by this decision

A small, read-only report of recurring review findings drawn from Mergeyard's own stored data. It needs no third-party credentials and creates nothing. File it separately if wanted.

## Prior requests

- #104: "[Post-M3] Propose issues from Sentry, PostHog and past review findings"
