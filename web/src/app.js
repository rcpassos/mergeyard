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
// stays open across the refresh, and focus inside it stays on the same control.
let openPills = [];
let pillFocus = null;
document.body.addEventListener('htmx:oobBeforeSwap', (event) => {
  if (event.detail.target.id !== 'harness-pills') return;
  openPills = [...event.detail.target.querySelectorAll('details[open]')].map((pill) => pill.id);
  pillFocus = captureFocus(event.detail.target);
});
document.body.addEventListener('htmx:oobAfterSwap', (event) => {
  if (event.detail.target.id !== 'harness-pills') return;
  for (const id of openPills) document.getElementById(id)?.setAttribute('open', '');
  restoreFocus(event.detail.target, pillFocus);
});

// Live updates replace whole nodes, so the browser forgets where the page was,
// which disclosures were open and what was focused. Remember those around each
// live swap and restore them once the new content is in place. Phase output is
// remembered for every content swap so its pinning survives actions too. The
// retry note also carries hx-preserve, but keeping its text here makes several
// live swaps in a row safe even if preservation is unavailable.
const swapSnapshots = new Map();

// Describe an element from the nearest stable ID downwards. IDs survive the
// swap; structure inside them does too, which covers a focused summary or a
// button without an ID of its own.
function selectorFor(element) {
  const parts = [];
  for (let node = element; node && node !== document.body; node = node.parentElement) {
    if (node.id) {
      parts.unshift(`#${CSS.escape(node.id)}`);
      return parts.join(' > ');
    }
    let index = 1;
    for (let sibling = node.previousElementSibling; sibling; sibling = sibling.previousElementSibling) {
      if (sibling.tagName === node.tagName) index += 1;
    }
    parts.unshift(`${node.tagName.toLowerCase()}:nth-of-type(${index})`);
  }
  return '';
}

// The nearest ID is the stable container the selector starts from.
function anchorFor(element) {
  for (let node = element; node && node !== document.body; node = node.parentElement) {
    if (node.id) return node.id;
  }
  return '';
}

function captureFocus(root) {
  const element = document.activeElement;
  if (!element || element === document.body || !root.contains(element)) return null;
  const snapshot = { element, selector: selectorFor(element), anchor: anchorFor(element) };
  if (typeof element.selectionStart === 'number') {
    snapshot.start = element.selectionStart;
    snapshot.end = element.selectionEnd;
    snapshot.direction = element.selectionDirection;
  }
  return snapshot;
}

// The selector addresses the old position, which another control can occupy
// when the run's available actions change. Only restore focus to the same kind
// of control that carried the identifying attributes. A path that starts at the
// page container needs the text check too; paths inside a stable ID do not,
// because their labels legitimately change with live state.
function sameControl(candidate, original, anchor) {
  if (candidate.tagName !== original.tagName) return false;
  let identified = false;
  for (const attribute of ['href', 'form', 'name', 'data-copy']) {
    const value = original.getAttribute(attribute);
    if (value === null) continue;
    identified = true;
    if (candidate.getAttribute(attribute) !== value) return false;
  }
  const form = original.closest('form[action]');
  if (form) {
    identified = true;
    if (candidate.closest('form[action]')?.getAttribute('action') !== form.getAttribute('action')) return false;
  }
  return identified || anchor !== 'content' || candidate.textContent.trim() === original.textContent.trim();
}

// A control that survives the swap can still change position when another
// action disappears, leaving the old selector pointing at nothing. Its stable
// attributes identify it wherever it moved.
function findControl(root, original, anchor) {
  const identified = ['href', 'form', 'name', 'data-copy'].some((attribute) => original.hasAttribute(attribute)) ||
    original.closest('form[action]') !== null;
  if (!identified) return null;
  for (const candidate of root.querySelectorAll('a[href], area[href], button, input, select, textarea, summary, [tabindex], [contenteditable]')) {
    if (sameControl(candidate, original, anchor)) return candidate;
  }
  return null;
}

function restoreFocus(root, snapshot) {
  if (!snapshot?.element) return;
  let element = snapshot.selector ? document.querySelector(snapshot.selector) : null;
  if (!element || !root.contains(element) || !sameControl(element, snapshot.element, snapshot.anchor)) {
    element = findControl(root, snapshot.element, snapshot.anchor);
  }
  if (!element) return;
  element.focus({ preventScroll: true });
  if (snapshot.start != null && element.setSelectionRange) {
    try {
      element.setSelectionRange(snapshot.start, snapshot.end, snapshot.direction);
    } catch {
      // Selection ranges only apply to text controls.
    }
  }
}

function outputPre(root) {
  const section = root.id === 'phase-output' ? root : root.querySelector('#phase-output');
  return section?.querySelector('pre') || null;
}

function outputPinned(pre) {
  // A few pixels of rounding should still count as the bottom.
  return pre.scrollHeight - pre.scrollTop - pre.clientHeight <= 4;
}

function pinOutput(pre) {
  if (pre) pre.scrollTop = pre.scrollHeight;
}

function captureOutput(root) {
  const pre = outputPre(root);
  return pre && { top: pre.scrollTop, pinned: outputPinned(pre) };
}

function captureSwap(root, live) {
  const note = root.querySelector('textarea[name="note"]');
  return {
    page: live && {
      scrollX: window.scrollX,
      scrollY: window.scrollY,
      disclosures: [...root.querySelectorAll('details[id]')].map((details) => [details.id, details.open]),
      focus: captureFocus(root),
      note: note && { id: note.id, value: note.value },
    },
    output: captureOutput(root),
  };
}

function restoreSwap(root, snapshot) {
  if (snapshot.page) {
    for (const [id, open] of snapshot.page.disclosures) {
      const details = document.getElementById(id);
      if (details?.tagName === 'DETAILS' && root.contains(details)) details.open = open;
    }
    if (snapshot.page.note) {
      const note = document.getElementById(snapshot.page.note.id);
      if (note && root.contains(note)) note.value = snapshot.page.note.value;
    }
  }
  const pre = outputPre(root);
  if (pre) {
    if (snapshot.output) {
      const bottom = Math.max(0, pre.scrollHeight - pre.clientHeight);
      pre.scrollTop = snapshot.output.pinned ? bottom : Math.min(snapshot.output.top, bottom);
    } else {
      // A newly rendered output starts at its newest line.
      pinOutput(pre);
    }
  }
  restoreFocus(root, snapshot.page?.focus);
  if (snapshot.page) window.scrollTo(snapshot.page.scrollX, snapshot.page.scrollY);
}

document.body.addEventListener('htmx:beforeSwap', (event) => {
  const root = event.detail.target;
  if (!root || (root.id !== 'content' && root.id !== 'phase-output')) return;
  const live = root.id === 'content' && event.detail.requestConfig?.triggeringEvent?.type === 'dashboard-update';
  swapSnapshots.set(root.id, captureSwap(root, live));
});
document.body.addEventListener('htmx:afterSwap', (event) => {
  const id = event.detail.target?.id;
  if (!id) return;
  const snapshot = swapSnapshots.get(id);
  if (!snapshot) return;
  swapSnapshots.delete(id);
  // An outerHTML swap reports the detached old element, so look the new one up.
  const root = document.getElementById(id);
  if (root) restoreSwap(root, snapshot);
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

// Destructive or quota-consuming forms open the confirmation dialog named by
// their data-confirm attribute before htmx sends anything. The dialog is a
// native <dialog>: the browser traps focus while it is open, Escape cancels,
// and closing returns focus to the button that submitted the form. A browser
// without this script never reaches this path and submits the unchanged form
// directly, so every action still works.
let pendingConfirm = null;
// HTMX keeps this context when retrying a queued request, but loses the
// issueRequest(true) flag. Remember acceptance for this request only; a new
// submission gets a new context and must ask again. Weak references also let
// discarded queue entries go without leaving acceptance on the form.
const acceptedRequests = new WeakSet();
let refreshDeferred = false;

// A modal is closed by a choice, Escape, or a programmatic dismissal. Focus
// always returns to the button that opened it; only an accepted choice issues
// the request htmx held back.
function settleConfirm(dialog, accepted) {
  if (!pendingConfirm || pendingConfirm.dialog !== dialog) return;
  const { issueRequest, trigger, request } = pendingConfirm;
  pendingConfirm = null;
  if (trigger?.isConnected) trigger.focus();
  if (accepted) {
    acceptedRequests.add(request);
    issueRequest(true);
  }
  if (refreshDeferred) {
    refreshDeferred = false;
    refresh();
  }
}

document.body.addEventListener('htmx:confirm', (event) => {
  const confirmable = event.detail.triggeringEvent?.target?.closest?.('[data-confirm]') || event.detail.elt?.closest?.('[data-confirm]');
  const selector = confirmable?.dataset.confirm;
  if (!selector) return;
  if (acceptedRequests.has(event.detail.etc)) return;
  event.preventDefault();
  const dialog = document.querySelector(selector);
  if (!dialog) {
    // Never let a destructive action through just because its confirmation
    // markup is missing or was swapped away.
    showToast('error', 'The confirmation dialog is missing. Refresh the page and try again.');
    return;
  }
  dialog.returnValue = '';
  pendingConfirm = { dialog, issueRequest: event.detail.issueRequest, trigger: event.detail.triggeringEvent?.submitter, request: event.detail.etc };
  dialog.showModal();
  // Some browsers leave focus behind when a dialog opens; start on Cancel so
  // the safe choice is active and Tab stays inside the modal.
  dialog.querySelector('button')?.focus();
});

// A live refresh already in flight when the dialog opened must not replace it.
document.body.addEventListener('htmx:beforeSwap', (event) => {
  if (!pendingConfirm) return;
  const target = event.detail.target?.id;
  if (target !== 'content' && target !== 'harness-pills') return;
  event.detail.shouldSwap = false;
  refreshDeferred = true;
});

document.body.addEventListener('click', (event) => {
  const choice = event.target.closest?.('[data-confirm-accept], [data-confirm-cancel]');
  if (!choice) return;
  const dialog = choice.closest('dialog');
  if (!dialog || !pendingConfirm || dialog !== pendingConfirm.dialog) return;
  event.preventDefault();
  const accepted = choice.hasAttribute('data-confirm-accept');
  dialog.close(accepted ? 'confirm' : 'cancel');
  settleConfirm(dialog, accepted);
});

document.addEventListener('close', (event) => settleConfirm(event.target, false), true);

let refreshTimer;
function refresh() {
  clearTimeout(refreshTimer);
  // A live swap would remove an open confirmation and its trigger. Hold the
  // refresh until the modal closes, then show the latest state.
  if (pendingConfirm) {
    refreshDeferred = true;
    return;
  }
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

// A freshly rendered phase output opens at its newest line. Polling keeps it
// there until the owner scrolls up.
pinOutput(outputPre(document));

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

// The category selection stays with this run across live content swaps. A
// delegated listener also handles chips inserted by the first owner-page load.
let timelineFilter = 'all';
let timelinePath = location.pathname;
function filterTimeline() {
  if (timelinePath !== location.pathname) {
    timelineFilter = 'all';
    timelinePath = location.pathname;
  }
  const timeline = document.querySelector('#run-timeline');
  if (!timeline) return;
  for (const button of timeline.querySelectorAll('[data-timeline-filter]')) {
    const selected = button.dataset.timelineFilter === timelineFilter;
    button.setAttribute('aria-pressed', String(selected));
    button.classList.toggle('btn-primary', selected);
    button.classList.toggle('btn-outline', !selected);
  }
  const entries = [...timeline.querySelectorAll('[data-timeline-event]')];
  for (const entry of entries) {
    entry.hidden = timelineFilter !== 'all' && entry.dataset.category !== timelineFilter;
  }
  timeline.querySelector('[data-timeline-empty]').hidden = timelineFilter === 'all' || entries.some((entry) => !entry.hidden);
}

document.body.addEventListener('click', (event) => {
  const button = event.target.closest('[data-timeline-filter]');
  if (!button) return;
  timelineFilter = button.dataset.timelineFilter;
  filterTimeline();
});
document.body.addEventListener('htmx:load', filterTimeline);
filterTimeline();
