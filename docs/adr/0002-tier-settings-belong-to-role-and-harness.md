# Tier settings belong to a role and harness

For #102's label-selected tiers, each role defines `low`, `medium`, and `high` settings separately for each harness it uses. Resolution selects the harness first, then that harness's tier settings. This lets tier labels compose with harness overrides while keeping harness-specific model and effort values together; refusing every tier combined with a switch away from the configured harness would limit explicit per-issue control.

Implementer and reviewer tier labels are independent and use the fixed, case-insensitive forms `mergeyard:implementer-tier=<tier>` and `mergeyard:reviewer-tier=<tier>`. A role with no tier label retains its existing resolution behavior. Labels selecting different tiers for the same role refuse the claim rather than silently selecting a potentially more expensive model; repeated labels selecting the same tier are allowed.

Each selected tier replaces model and effort together. An omitted or `null` model or effort uses the selected harness's default rather than inheriting the role's ordinary model or effort. Partial tier definitions are allowed; selecting an undefined tier for the resolved role and harness refuses that issue's claim, applies needs-attention, and posts one explanation. A tier label with no configured tiers is also an undefined selection.

A repository's supplied tier map replaces the entire inherited map for that role. Omitting the map inherits the global map, while `{}` clears it. This keeps the effective tier definitions explicit instead of mixing repository entries with hidden global entries.

The run stores each role's selected tier and matching labels alongside the resolved role settings. They are visible in plan, status, dashboard run detail, and the PR footer. Tier selection follows ADR 0001: settings are resolved at claim and remain fixed for the run. The classifier and automatic escalation are deferred from this slice.
