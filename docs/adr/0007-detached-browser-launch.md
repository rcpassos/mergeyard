# The Linux browser opens detached from Mergeyard

The handoff page (ADR-0006) opens the dashboard by launching the browser
explicitly, so the token never reaches a process command line. When the
launcher ran the browser itself — the selected desktop entry's command, or
`sensible-browser` / `x-www-browser` — that browser became a child of
Mergeyard: `mergeyard open` blocked until the user closed it, Ctrl-C killed it,
and `mergeyard start` killed it when the runtime shut down. The browser belongs
to the user, not to the command that opened it.

On Linux, every browser launcher now runs detached: it starts in its own
session and process group, without Mergeyard's context, and Mergeyard never
waits for it. The command returns as soon as a launcher starts. A launcher that
cannot start is reported and the next fallback is tried; when no launcher
starts, every failure is part of the reported error.

macOS keeps its AppleScript path: LaunchServices starts the browser, so it is
never a child of Mergeyard, and the script returns as soon as the request is
handed over.

## Considered Options

- **Wait for the handoff launchers and detach only the direct browser.**
  Rejected: `gio` and `gtk-launch` hand off, but nothing guarantees they
  return, so the command could still block, and shutdown would still kill a
  launcher it started.
- **Wait for the browser with a deadline, then detach it.** Rejected: the
  command would still stall for the deadline on every launch, and a browser
  that outlives the deadline would still be killed at shutdown.
- **Kill the browser when the command ends.** Rejected: the user opened the
  dashboard to keep watching it; the browser must outlive the command.
- **Double-fork so the browser has no parent at all.** Rejected: a session and
  process group of its own already detach the browser from Mergeyard's
  signals and lifetime, and the extra helper process buys nothing a user or
  the runtime can observe.
- **Run the browser through the shared process policy.** Rejected: that policy
  owns the process group and stops it when the command ends, which is the
  opposite of what a browser needs.

## Consequences

- `mergeyard open` returns immediately with every Linux launcher, and the
  browser stays open after the CLI exits.
- Stopping `mergeyard start` never terminates a browser it launched.
- A launcher that starts and then fails is no longer observed, because
  Mergeyard does not wait for it. Only a launcher that cannot start is
  reported; the previous fallback is tried instead.
- The Linux launchers run outside Mergeyard's terminal session, so a Ctrl-C in
  the terminal does not reach the browser.
