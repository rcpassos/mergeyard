# Availability follows the restricted resource

OpenCode can use a Go subscription allowance, prepaid credits, or both through its optional Extra Usage setting. A harness-wide block would pause independently available work, while treating every provider as independent would miss shared balances. Availability restrictions and recovery probes therefore belong to the affected allowance, balance, or budget; a recovery result can clear only the scope it actually exercised. Existing single-account harnesses may retain one harness-wide scope.

Verified scheduled resets, including subscription allowances and monthly budget caps, use timed waits. Depleted credits and caps without scheduled recovery require explicit recovery; unknown errors retain ordinary failure handling. This refines the earlier rule that grouped all spend caps with exhausted credits. Mergeyard honors OpenCode's existing Extra Usage setting but does not enable it or switch a run's provider/model automatically.

This decision describes the planned OpenCode integration. Current runtime restrictions and recovery probes remain harness-wide until the scoped model is implemented. The design must preserve shared-resource identity, persist scope across restarts, and target recovery at the restricted resource rather than clearing a block after an unrelated successful request.
