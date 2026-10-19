# The dashboard owner session is origin-scoped, not a cookie

The dashboard owner session began as a cookie whose name carried the runtime's
port, which kept two runtimes on one host from overwriting each other. That does
not isolate them. Cookies are keyed by host, not port (RFC 6265 §8.5), and
SameSite treats every port on a host as the same site. On a machine shared with
other users, a page served from any other loopback port receives the cookie;
its server can then load the dashboard, read the CSRF token, and forge a retry
note that becomes human guidance for an agent running with bypass permissions.
Stop, pause, takeover and handback are reachable the same way, and the session
lives until the runtime restarts.

The one-time dashboard link now hands the session to the browser in the
redirect fragment. The dashboard script moves it into origin-scoped storage and
attaches it to dashboard requests as the `X-Mergeyard-Session` header. The
runtime sets no cookie. Browser storage and headers are scoped to the full
origin (scheme, host and port), so another loopback port receives neither the
credential nor a cookie to replay. The runtime's Host check and the session's
recorded exchange authority both refuse it at any other authority. State
changes still require the exact `Origin` and the CSRF token. A navigation
without the header gets the lock layout plus the script that attaches the
stored session.

## Considered Options

- **Give each runtime its own loopback hostname or address.** Rejected: any
  local user can bind another port on the same address or resolve the same
  `.localhost` name, so the cookie still reaches them. Only the browser's
  origin key is enforced by the browser, and it ignores ports for cookies.
- **Keep the cookie for pages and require an origin-scoped token for
  mutations.** Rejected: a stolen cookie still loads owner pages and leaks run
  and log data, and two credentials with different lifetimes invite mistakes.
- **Put the session in a path or query so no cookie travels.** Rejected: URL
  credentials leak through history, logs and copied links, and the browser does
  not scope them to the runtime.
- **Rely on HTTP Basic auth, whose protection space is origin-scoped.**
  Rejected: embedded credentials are unreliable across browsers and still
  surface in history.

## Consequences

- The impostor-port test asserts the exchange sets no cookie, a shared cookie
  jar replays nothing to a second loopback port, and a cookie-shaped credential
  cannot unlock the dashboard.
- The dashboard needs JavaScript to attach the header. The CLI remains the
  script-free control path, and the lock page says so.
- The owner session is no longer `HttpOnly`: script can read it, and unlike the
  cookie it survives a browser restart in origin-scoped storage. It still dies
  with the runtime process and only reaches the exact origin, and the service
  sets a strict `script-src 'self'` CSP.
- A restarted runtime rotates the session, so a stale stored value locks out
  and `mergeyard open` hands over a fresh one. Tokens remain single-use and
  short-lived, so a stolen link only races the browser once. The link reaches
  the browser through an owner-only handoff page instead of a process argument
  (ADR-0005).
