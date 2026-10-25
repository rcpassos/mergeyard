# Resolve OpenCode's effective default model at claim

Existing role snapshots preserve configured model strings, including an empty string that delegates model choice to mutable harness defaults. OpenCode's funding-scoped availability and recovery need a reliable provider/model identity. For OpenCode, resolve an omitted model to the effective provider/model when claiming the issue, preserve that identity for later phases and recovery, and refuse the claim with an actionable explanation if it cannot be resolved.

This extends ADR 0001 for the planned OpenCode integration. Requiring an explicit model everywhere would remove convenient defaults; continuing to consult mutable defaults could move an existing run to a different provider and make its recovery check exercise the wrong resource. Tier settings that select the harness default still follow ADR 0002, with OpenCode's selected default resolved at claim.
