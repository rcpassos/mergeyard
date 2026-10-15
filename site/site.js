const descriptions = {
  issue: 'Add ready-for-agent to an approved GitHub issue. Mergeyard claims eligible work and prepares an isolated Git worktree.',
  implement: 'Your chosen implementer builds the change in its own worktree. Mergeyard opens a draft pull request for independent review.',
  review: 'A separate agent reviews the PR. Blocking findings return to the implementer for fixes and re-review, up to the configured round limit.',
  ci: 'After review approval, Mergeyard waits for CI checks. Failed checks can enter a bounded repair loop before the PR is ready.',
  merge: 'The reviewed PR is ready when CI passes. Inspect the work and merge it yourself. The final decision stays with you.',
};

const stages = document.querySelectorAll('[data-stage]');
const detail = document.querySelector('#stage-detail');
const detailPanel = document.querySelector('.map-detail');

// Keep the links useful without JavaScript; enhance them into demo controls.
for (const stage of stages) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = stage.className;
  button.dataset.stage = stage.dataset.stage;
  button.setAttribute('aria-controls', 'stage-detail');
  button.append(...stage.childNodes);
  stage.replaceWith(button);
}
const controls = document.querySelectorAll('button[data-stage]');
function updateStage(selected) {
  for (const control of controls) {
    if (control === selected) control.setAttribute('aria-current', 'step');
    else control.removeAttribute('aria-current');
  }
  detail.textContent = descriptions[selected.dataset.stage];
}
for (const control of controls) control.addEventListener('click', () => updateStage(control));
detailPanel.hidden = false;
updateStage(controls[2]);

const copyButton = document.querySelector('.copy-button');
const copyStatus = document.querySelector('.copy-status');
if (navigator.clipboard?.writeText) {
  copyButton.hidden = false;
  copyButton.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(document.querySelector('#setup-commands').textContent);
      copyStatus.textContent = 'Commands copied.';
    } catch {
      copyStatus.textContent = 'Copy unavailable. Select the commands above to copy them manually.';
    }
  });
}
