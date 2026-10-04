const status = document.querySelector('#connection');
const events = new EventSource('/events');
events.onopen = () => {
  status.textContent = 'Live updates connected';
  htmx.ajax('GET', '/scheduler', { target: '#scheduler', swap: 'outerHTML' });
};
events.onerror = () => { status.textContent = 'Reconnecting to live updates…'; };
for (const type of ['scheduler.paused', 'scheduler.resumed']) {
  events.addEventListener(type, () => {
    htmx.ajax('GET', '/scheduler', { target: '#scheduler', swap: 'outerHTML' });
  });
}
document.body.addEventListener('htmx:responseError', (event) => {
  const code = event.detail.xhr.getResponseHeader('X-Mergeyard-Error-Code');
  document.querySelector('#action-error').textContent = code
    ? `${code}: The scheduler action failed. Check the application log for details.`
    : 'The scheduler action failed. Refresh the page and try again.';
});
