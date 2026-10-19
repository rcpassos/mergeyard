# The dashboard link reaches the browser through an owner-only handoff page

`mergeyard start` and `mergeyard open` launched the browser with the dashboard
URL as an argument. On a machine shared with other users, `ps -axww` on macOS
and the world-readable `/proc/<pid>/cmdline` on Linux expose that argument. A
polling attacker could exchange the single-use token before the browser did,
take the owner session, and leave the owner with an expired-link message.

The CLI now writes a small handoff page into `<workspace>/.dashboard-links/`,
with an owner-only directory (`0700`) and page (`0600`), and passes only that
path to `open` or `xdg-open`. The browser follows the page's redirect to the
single-use link. The token therefore never appears in a process command line; a
local user who reads the launcher arguments sees a path they cannot read. The
page holds only the link token, never the owner session; ADR-0004 still keeps
the session out of URLs. A later launch removes the page once the token
lifetime has passed.

## Considered Options

- **Pass the URL in an environment variable to the launcher.** Rejected: the
  launcher expands it into its own argument vector before starting the browser,
  so the token still becomes visible as an argument.
- **Use a platform-specific AppleScript or desktop-entry launcher.** Rejected:
  it adds macOS/Linux branches for the same handoff when the redirect page works
  on both.
- **Open the dashboard URL directly and rely on the token's single use.**
  Rejected: the attacker only has to poll faster than the browser, and the
  owner loses the session.
- **Pipe the URL to the launcher on stdin.** Rejected: `open` and `xdg-open` do
  not accept a URL on stdin.

## Consequences

- `mergeyard start` and `mergeyard open` still take one command and land the
  owner signed in; the extra redirect is local and immediate.
- The handoff page lives under `<workspace>/.dashboard-links/` and is readable
  only by its owner. A later launch removes pages once their token lifetime has
  passed, so an old page cannot be replayed.
- Expired and reused links keep the existing message pointing at
  `mergeyard open`.
- Tests launch a fake browser and assert that its arguments are the handoff
  path with no token or token-bearing URL, and that the handoff directory and
  page are `0700` and `0600`.
