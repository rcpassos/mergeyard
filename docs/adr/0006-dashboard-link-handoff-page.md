# The dashboard link reaches the browser without entering process arguments

`mergeyard start` and `mergeyard open` launched the browser with the dashboard
URL as an argument. On a machine shared with other users, `ps -axww` on macOS
and the world-readable `/proc/<pid>/cmdline` on Linux expose that argument. A
polling attacker could exchange the single-use token before the browser did,
take the owner session, and leave the owner with an expired-link message.

The CLI now keeps the token out of every process command line while still
selecting the HTTP browser rather than the HTML file association:

- On macOS, `osascript` receives `open location "<url>"` over stdin. LaunchServices
  resolves the default HTTP browser; the script and URL never appear in
  arguments.
- On Linux, a handoff page is written under
  `$HOME/mergeyard-dashboard-handoff/` with an owner-only directory (`0700`)
  and page (`0600`). The directory is not hidden, so confined browsers such as
  the Firefox snap can read it through the snap `home` interface. The CLI
  resolves the default web browser with `xdg-settings` or
  `x-scheme-handler/http`, then launches it explicitly with `gio launch`,
  `gtk-launch`, or the browser command from the selected desktop entry and only
  the page path. The page is never handed to `xdg-open`, which would use the
  HTML association. If no HTTP browser can be resolved, the command reports the
  missing launcher and leaves the printed link. On Linux every launcher runs
  detached from Mergeyard (ADR-0007).

The browser follows the page's redirect to the single-use link. The token
therefore never appears in a process command line; a local user who reads the
launcher arguments sees a path they cannot read. The page holds only the link
token, never the owner session; ADR-0005 still keeps the session out of URLs. A
later launch removes the page once the token lifetime has passed.

## Considered Options

- **Pass the URL in an environment variable to the launcher.** Rejected: the
  launcher expands it into its own argument vector before starting the browser,
  so the token still becomes visible as an argument.
- **Use `open`/`xdg-open` on the handoff page without selecting an
  application.** Rejected: that uses the file association for `text/html`, which
  may be an editor, while HTTP URLs keep a separate web-browser association. A
  last-resort fallback to `xdg-open` would reintroduce that failure whenever the
  desktop helper tools are absent.
- **Keep the handoff under the default hidden workspace.** Rejected: the snap
  `home` interface excludes hidden home paths, so a confined Firefox cannot read
  the page.
- **Mediate the file through the XDG document portal.** Rejected for now: it
  adds a D-Bus dependency and a desktop-portal requirement for a redirect page
  that a non-hidden owner-only home directory already makes readable.
- **Open the dashboard URL directly and rely on the token's single use.**
  Rejected: the attacker only has to poll faster than the browser, and the
  owner loses the session.
- **Pipe the URL to the launcher on stdin.** Rejected: `open` and `xdg-open` do
  not accept a URL on stdin.

## Consequences

- `mergeyard start` and `mergeyard open` still take one command and land the
  owner signed in; the extra redirect is local and immediate.
- Linux handoff pages live in a non-hidden, owner-only home directory so a
  confined browser can read them. Other platforms use
  `<workspace>/.dashboard-links/` when they cannot use the macOS HTTP-handler
  path. A later launch removes pages once their token lifetime has passed.
- Expired and reused links keep the existing message pointing at
  `mergeyard open`.
- Tests launch a fake browser and assert that its arguments are the handoff
  path or AppleScript stdin marker with no token or token-bearing URL, that the
  default web browser is selected rather than the HTML handler even when
  `gio`/`gtk-launch` are absent, and that the handoff directory and page are
  `0700` and `0600` in a Snap-accessible location.
