const status = document.querySelector('#connection');
const events = new EventSource('/events');
let refreshTimer;
function refresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => htmx.trigger(document.body, 'dashboard-update'), 100);
}
events.onopen = () => {
  status.textContent = 'Live updates connected';
  refresh();
};
events.onerror = () => { status.textContent = 'Reconnecting to live updates…'; };
for (const type of [
  'scheduler.paused', 'scheduler.resumed', 'harness.usage_limited', 'harness.credits_exhausted', 'harness.credit_recovered', 'harness.probe_reserved', 'harness.probe_released', 'harness.available',
  'run.claimed', 'run.preparing', 'run.needs_attention', 'run.stop_requested', 'run.stopped', 'run.failed',
  'run.takeover_requested', 'run.retry_requested', 'run.retry_rejected', 'review.approval_invalidated',
  'run.handback_requested', 'run.handback_inspected', 'run.handback_committed', 'run.handback_pending', 'run.completed', 'run.manual', 'run.handed_back', 'run.waiting_for_harness',
  'publication.pending', 'publication.warning', 'publication.published',
  'pr.merge_observed', 'merge.cleanup_updated', 'ci.updated', 'pr.readiness_started', 'review.completed', 'review.restored', 'fix.completed', 'fix.committed', 'fix.pushed', 'phase.started', 'phase.attempt_started', 'harness.session_discovered', 'harness.session_resume_failed', 'phase.completed', 'phase.failed', 'pr.created', 'pr.ready_for_review',
]) {
  events.addEventListener(type, refresh);
}
events.addEventListener('queue.updated', () => {
  if (location.pathname === '/' || location.pathname === '/queue') refresh();
});
events.addEventListener('diagnostics.updated', () => {
  if (location.pathname === '/settings') refresh();
});
document.body.addEventListener('htmx:responseError', (event) => {
  const code = event.detail.xhr.getResponseHeader('X-Mergeyard-Error-Code');
  document.querySelector('#action-error').textContent = (code?.startsWith('takeover.') || code?.startsWith('handback.'))
    ? event.detail.xhr.responseText.trim()
    : code
    ? `${code}: The action failed. Check the application log for details.`
    : 'The request failed. Refresh the page and try again.';
});
document.body.addEventListener('htmx:afterRequest', (event) => {
  if (event.detail.successful && event.detail.requestConfig.verb === 'post') {
    document.querySelector('#action-error').textContent = '';
    refresh();
  }
});
document.body.addEventListener('click', async (event) => {
  const button = event.target.closest('[data-copy]');
  if (!button) return;
  const message = document.querySelector('#copy-status');
  try {
    await navigator.clipboard.writeText(button.dataset.copy);
    message.textContent = 'Copied to clipboard.';
  } catch {
    message.textContent = `Copy this text: ${button.dataset.copy}`;
  }
});
