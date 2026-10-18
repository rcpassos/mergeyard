# The CLI authenticates the runtime with a workspace secret

The runtime answers the CLI on a loopback port. Any local user can reach that
port and, whenever the runtime is down, bind it first. The CLI previously
identified the runtime only by the workspace path in the `/api/status` answer,
which is easy to guess, and then executed the takeover command the answer
supplied. An impostor could therefore run an arbitrary binary as the user, and
`/api/status` handed the browser CSRF token to any local process.

The runtime and CLI now share a workspace secret (issue #161) written to
`<workspace>/runtime.secret` with owner-only permissions (`0600`). Every `/api`
request carries a fresh nonce, the request time, and an HMAC-SHA256 proof of the
secret over method, target, nonce and time. The runtime rejects unproven API
requests with `runtime.unauthenticated` and proves the same secret in its
response, and the CLI refuses a reply that cannot prove it. The secret itself
never crosses the port. `mergeyard start` rotates the secret on every startup.
The CLI rebuilds the interactive takeover invocation from the run's stored role
settings, the recorded implementer session and the managed worktree layout; the
runtime returns only the prepared run snapshot.

## Considered Options

- **Send the secret as a plain header.** Rejected: the CLI would disclose the
  secret to whichever process holds the port, and it could not tell an impostor
  from the runtime.
- **Trust the loopback address or the reported workspace path alone.** Rejected:
  both are available to every local user.
- **Move the CLI API to a Unix socket with owner-only permissions.** Rejected:
  the dashboard and CLI already share one HTTP listener, configuration carries a
  port, and the secret keeps one boundary for both.
- **Require the secret for the browser dashboard too.** Rejected: a browser
  cannot read the workspace file. The dashboard keeps its origin and CSRF
  checks, and the CSRF token is now only handed out to a proven caller.
- **Keep returning the takeover command from the API.** Rejected: even a proven
  runtime would then choose what the user's machine executes. The CLI owns that
  decision.

## Consequences

- The first command after deleting the secret reports `runtime.secret_missing`
  and points at `mergeyard start`; a group- or world-readable file reports
  `runtime.secret_insecure` with the `chmod 600` fix; a port that cannot prove
  the secret reports `runtime.unauthenticated` and no control is attempted.
- Restart rotates the secret. A CLI always re-reads the file, so only a long-
  running pre-restart process would be refused.
- A local process can still load the dashboard pages and act through the
  browser forms. Those checks protect against other websites, not against
  another user on the machine.
- CLI and runtime must be upgraded together: an older runtime cannot prove the
  secret, and the CLI refuses it with restart guidance.
