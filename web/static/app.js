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
document.body.addEventListener('htmx:responseError', () => {
  document.querySelector('#action-error').textContent = 'The scheduler action failed. Refresh the page and try again.';
});
