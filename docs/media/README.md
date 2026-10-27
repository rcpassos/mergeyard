# README demo media

`issue-to-merge.gif` and `run-detail.png` show the shipped dashboard templates
at a 1440-pixel desktop viewport. The issue, repository, agent output, review,
CI results, commits, and events are illustrative example data, not a captured
agent run or evidence that a real PR passed CI. Each image labels this explicitly.

The GIF walks through ready-label queueing, implementation, independent review,
a fix, re-review, CI, and the dashboard's **Waiting for your merge** group.
Stages last three seconds; the final dashboard lasts five. The PNG shows the
ready run with its CI results and timeline. Run facts and review details are
collapsed using the dashboard's disclosures.

When updating these assets, render the current templates in `web/templates/`
with example page data and `web/static/` assets, disable live refresh in the
capture, and retain the example-data banner. Capture each stage at the same
viewport, then encode a looping GIF. Do not include dashboard session links,
workspace secrets, real agent transcripts, or private repository information.
The website's workflow illustration is maintained separately in `site/`.
