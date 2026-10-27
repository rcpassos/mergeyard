# Usage limits and credits

Runs pause when a supported agent reports a usage limit and resume after its
reset time or configured cooldown. The dashboard groups them under **Waiting
for harness**. Unknown failures follow the ordinary attempt budget.

Claude detects the observed headless weekly usage-limit rejection and waits
until its reported reset or the configured cooldown before resuming. Codex
detects its captured Plus-plan same-day rejection, using the host-local reset
clock plus one minute or the configured cooldown. Unknown variants consume the
ordinary attempt budget.
Exhausted-credit detection and credit recovery controls stay unavailable for
both harnesses until their native evidence bindings land. See [recovery acceptance and evidence](m3-acceptance.md)
for the verified recovery core, current capability gates, and offline checks.

For wait budgets and cooldown settings, see
[configuration](configuration.md). For stopping, retrying,
or taking over a run, see [CLI operations](cli.md). Native captures and current
capability gates are documented in [recovery acceptance](m3-acceptance.md).
