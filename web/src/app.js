const SESSION_KEY = 'mergeyard.session';
const SESSION_HEADER = 'X-Mergeyard-Session';
const content = document.querySelector('#content');

// The live-update connection shows as a status dot; its text reaches
// assistive technology through a status region and sighted users as a tooltip.
const connectionStates = {
  connecting: ['status-neutral', 'Connecting to live updates…'],
  connected: ['status-success', 'Live updates connected'],
  reconnecting: ['status-warning', 'Reconnecting to live updates…'],
  locked: ['status-error', 'Dashboard locked'],
};
function showConnection(state) {
  const connection = document.querySelector('#connection');
  if (!connection) return;
  const [variant, text] = connectionStates[state];
  connection.dataset.state = state;
  connection.dataset.tip = text;
  connection.querySelector('.status').className = `status ${variant} size-3`;
  connection.querySelector('.sr-only').textContent = text;
}

// Toasts carry action feedback: a success dismisses itself, an error stays
// until dismissed and is announced as an alert.
function showToast(kind, text) {
  const region = document.querySelector('#toasts');
  if (!region) return;
  // A repeated failure, such as a live refresh failing on every event, keeps
  // one toast instead of stacking copies.
  if ([...region.children].some((toast) => toast.textContent.startsWith(text))) return;
  const toast = document.createElement('div');
  toast.className = `alert alert-${kind} max-w-sm`;
  toast.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  const message = document.createElement('p');
  message.className = '[overflow-wrap:anywhere]';
  message.textContent = text;
  toast.append(message);
  if (kind === 'error') {
    const dismiss = document.createElement('button');
    dismiss.type = 'button';
    dismiss.className = 'btn btn-ghost btn-xs';
    dismiss.textContent = 'Dismiss';
    dismiss.addEventListener('click', () => toast.remove());
    toast.append(dismiss);
  } else {
    setTimeout(() => toast.remove(), 3000);
  }
  region.append(toast);
}

// Live updates refresh the header pills out of band; an open pill dropdown
// stays open across the refresh.
let openPills = [];
document.body.addEventListener('htmx:oobBeforeSwap', (event) => {
  if (event.detail.target.id !== 'harness-pills') return;
  openPills = [...event.detail.target.querySelectorAll('details[open]')].map((pill) => pill.id);
});
document.body.addEventListener('htmx:oobAfterSwap', (event) => {
  if (event.detail.target.id !== 'harness-pills') return;
  for (const id of openPills) document.getElementById(id)?.setAttribute('open', '');
});

// A blocked clipboard opens a modal with the text selected for a manual copy.
function showCopyFallback(text) {
  const dialog = document.querySelector('#copy-fallback');
  if (!dialog) return;
  const field = dialog.querySelector('textarea');
  field.value = text;
  dialog.showModal();
  field.focus();
  field.select();
}

function readSession() {
  try {
    return localStorage.getItem(SESSION_KEY) || '';
  } catch {
    return '';
  }
}
function writeSession(value) {
  try {
    localStorage.setItem(SESSION_KEY, value);
  } catch {
    // Storage is unavailable; the dashboard stays locked.
  }
}
function clearSession() {
  try {
    localStorage.removeItem(SESSION_KEY);
  } catch {
    // Nothing to clear.
  }
}

// The unlock redirect carries the owner session in the URL fragment. Browsers
// never send fragments to a server and scope storage to the exact scheme, host
// and port, so no other loopback port can receive the session. Cookies cannot
// hold it: a 127.0.0.1 cookie is shared with every port on the host.
const fragment = new URLSearchParams(location.hash.slice(1));
const exchanged = fragment.get('session');
if (exchanged) {
  writeSession(exchanged);
  history.replaceState(null, '', location.pathname + location.search);
}
let session = readSession();

// A rejected request carries the credential this tab has been using since it
// loaded. If shared storage already holds a different one, another tab
// exchanged a fresh link while the request was in flight: adopt it by
// reloading instead of deleting the shared credential.
function lockDashboard(rejected) {
  const stored = readSession();
  if (stored && stored !== rejected) {
    location.reload();
    return;
  }
  clearSession();
  session = '';
  showConnection('locked');
  location.reload();
}

document.body.addEventListener('htmx:configRequest', (event) => {
  if (session) event.detail.headers[SESSION_HEADER] = session;
});

let refreshTimer;
function refresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => htmx.trigger(document.body, 'dashboard-update'), 100);
}

const refreshEvents = [
  'scheduler.paused', 'scheduler.resumed', 'harness.usage_limited', 'harness.credits_exhausted', 'harness.credit_recovered', 'harness.probe_reserved', 'harness.probe_released', 'harness.available', 'harness.check_started', 'harness.check_updated', 'harness.check_completed',
  'run.claimed', 'run.preparing', 'run.needs_attention', 'run.stop_requested', 'run.stop_leftovers', 'run.stopped', 'run.failed',
  'run.takeover_requested', 'run.retry_requested', 'run.retry_rejected', 'review.approval_invalidated',
  'run.handback_requested', 'run.handback_inspected', 'run.handback_committed', 'run.handback_pending', 'run.completed', 'run.manual', 'run.handed_back', 'run.waiting_for_harness',
  'publication.pending', 'publication.warning', 'publication.published', 'publication.stopped', 'publication.requeued',
  'pr.merge_observed', 'merge.cleanup_updated', 'ci.updated', 'escalation.updated', 'pr.readiness_started', 'review.completed', 'review.restored', 'fix.completed', 'fix.committed', 'fix.pushed', 'phase.started', 'phase.attempt_started', 'harness.session_discovered', 'harness.session_resume_failed', 'phase.completed', 'phase.failed', 'pr.created', 'pr.ready_for_review',
  'reconcile.repository_failed', 'reconcile.issue_failed', 'reconcile.run_failed',
];

let streamController = null;
let lastEventID = '';

function handleFrame(frame) {
  let name = 'message';
  let hasData = false;
  for (const raw of frame.split('\n')) {
    const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    if (line.startsWith(':')) continue;
    if (line.startsWith('id:')) {
      lastEventID = line.slice(3).trim();
      continue;
    }
    if (line.startsWith('event:')) {
      name = line.slice(6).trim();
      continue;
    }
    if (line.startsWith('data:')) hasData = true;
  }
  if (!hasData) return;
  if (name === 'queue.updated') {
    if (location.pathname === '/' || location.pathname === '/queue') refresh();
    return;
  }
  if (name === 'diagnostics.updated') {
    if (location.pathname === '/settings') refresh();
    return;
  }
  if (refreshEvents.includes(name)) refresh();
}

async function startEvents() {
  if (streamController || !session) return;
  const controller = new AbortController();
  streamController = controller;
  const sent = session;
  const headers = { [SESSION_HEADER]: sent };
  if (lastEventID) headers['Last-Event-ID'] = lastEventID;
  try {
    const response = await fetch('/events', { headers, cache: 'no-store', signal: controller.signal });
    if (response.status === 403) {
      streamController = null;
      lockDashboard(sent);
      return;
    }
    if (!response.ok || !response.body) throw new Error(`live updates: ${response.status}`);
    showConnection('connected');
    refresh();
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let split;
      while ((split = buffer.indexOf('\n\n')) >= 0) {
        handleFrame(buffer.slice(0, split));
        buffer = buffer.slice(split + 2);
      }
    }
  } catch {
    if (controller.signal.aborted) return;
  }
  streamController = null;
  if (!session) return;
  showConnection('reconnecting');
  setTimeout(startEvents, 2000);
}

document.body.addEventListener('htmx:afterSwap', () => {
  const current = document.querySelector('#content');
  if (!current) return;
  if (current.hasAttribute('data-locked')) {
    lockDashboard(session);
    return;
  }
  startEvents();
});

// A browser navigation carries no session header, so the server answers with a
// boot page. The script attaches the stored session and loads the real content.
if (content && content.hasAttribute('data-boot')) {
  if (!session) {
    showConnection('locked');
  } else {
    htmx.ajax('GET', location.pathname + location.search, {
      target: '#content', swap: 'outerHTML', headers: { [SESSION_HEADER]: session },
    });
  }
} else if (content) {
  startEvents();
}

addEventListener('pageshow', (event) => {
  if (!event.persisted) return;
  streamController?.abort();
  streamController = null;
  const current = document.querySelector('#content');
  if (current && !current.hasAttribute('data-boot') && !current.hasAttribute('data-locked')) startEvents();
});

document.body.addEventListener('htmx:responseError', (event) => {
  const xhr = event.detail.xhr;
  if (xhr.status === 403 && xhr.responseText.includes('locked to the workspace owner')) {
    lockDashboard(session);
    return;
  }
  const code = xhr.getResponseHeader('X-Mergeyard-Error-Code');
  showToast('error', (xhr.status === 409 || code?.startsWith('takeover.') || code?.startsWith('handback.'))
    ? xhr.responseText.trim()
    : code
    ? `${code}: The action failed. Check the application log for details.`
    : 'The request failed. Refresh the page and try again.');
});
document.body.addEventListener('htmx:afterRequest', (event) => {
  if (event.detail.successful && event.detail.requestConfig.verb === 'post') {
    showToast('success', 'Action completed.');
    refresh();
  }
});
// The server renders each time as <time datetime> with a UTC fallback. Show
// them in the browser's locale and time zone: past times as relative text with
// the absolute time on hover, resets and deadlines as absolute plus relative.
const absoluteTime = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });
const clockTime = new Intl.DateTimeFormat(undefined, { timeStyle: 'short' });
const relativeTime = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto', style: 'short' });
const timeUnits = [['year', 31536000], ['month', 2592000], ['week', 604800], ['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]];

function relativeText(at) {
  const seconds = (at.getTime() - Date.now()) / 1000;
  for (const [unit, size] of timeUnits) {
    if (Math.abs(seconds) >= size) return relativeTime.format(Math.trunc(seconds / size), unit);
  }
  return relativeTime.format(0, 'second');
}

function formatTimes(root) {
  const elements = [...root.querySelectorAll('time[datetime]')];
  if (root.matches?.('time[datetime]')) elements.push(root);
  for (const element of elements) {
    const at = new Date(element.dateTime);
    if (Number.isNaN(at.getTime())) continue;
    const absolute = absoluteTime.format(at);
    element.title = absolute;
    if (element.dataset.time === 'until') {
      const today = at.toDateString() === new Date().toDateString();
      element.textContent = `${today ? clockTime.format(at) : absolute}, ${relativeText(at)}`;
    } else {
      element.textContent = relativeText(at);
    }
  }
}

formatTimes(document);
document.body.addEventListener('htmx:load', (event) => formatTimes(event.detail.elt));
setInterval(() => formatTimes(document), 30000);

document.body.addEventListener('click', async (event) => {
  const button = event.target.closest('[data-copy]');
  if (!button) return;
  try {
    await navigator.clipboard.writeText(button.dataset.copy);
    showToast('success', 'Copied to clipboard.');
  } catch {
    showCopyFallback(button.dataset.copy);
  }
});
