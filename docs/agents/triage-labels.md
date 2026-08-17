# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual strings used in this repo's issue tracker. Because issues are local markdown files, the string is written as a `Status:` line near the top of each issue file.

| Label in mattpocock/skills | Status string in our tracker | Meaning                                  |
| -------------------------- | ---------------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`               | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`                 | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`            | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`            | Requires human implementation            |
| `wontfix`                  | `wontfix`                    | Will not be actioned                     |
| —                          | `done`                       | Landed; no further action                |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), write the corresponding `Status:` string from this table.

`done` is local to this repo and has no counterpart in the skill vocabulary, because every role above is a *pre-implementation* triage state and none of them can say "this shipped". A skill will therefore never ask for it — but without it, a landed issue has to keep wearing the label it had while it was still work, which reads as a backlog that never drains. Set it when the work is merged.

Edit the right-hand column to match whatever vocabulary you actually use.
