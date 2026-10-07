# Mergeyard

Mergeyard coordinates coding agents to turn approved issues into reviewed, merge-ready pull requests. The user retains the merge decision.

## Language

**Usage limit**: A temporary restriction on a harness's available usage, with a reported reset time or an assumed cooldown.

**Limit signal**: A harness's native evidence that an execution ended because of a usage limit or exhausted credits, as distinct from the outcome Mergeyard classifies from it.
_Avoid_: Native limit format, limit error

**Exhausted-credit block**: A restriction caused by exhausted credits or a spend cap, whose recovery requires an explicit user action rather than a scheduled reset.

**Recovery probe**: A user-requested attempt to establish whether a harness blocked by exhausted credits is usable again. It can perform the next work for a selected run or make a minimal availability check without engineering work.
