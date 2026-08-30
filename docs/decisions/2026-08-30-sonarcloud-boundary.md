# SonarCloud caller boundary (#221)

Status: decided 2026-08-30 (cycle ticket `SCRUM-6`, task t4).
Disposition: keep machine-facing sweep queries separate from operator-facing
skill queries. This record closes issue #221; a `nodes sonar` verb remains
issue #218 territory.

## Decision

`examples/pr-upkeep/sweep.py` owns SonarCloud queries that are part of the
scheduled machine workflow. Those queries produce stable finding documents,
obey the sweep's repository grant and call budget, and participate in its
deduplication and routing semantics.

The `cicd` and `sonarclaude` skills own interactive, operator-facing queries.
They answer questions such as PR gate status, issue inspection, metrics,
hotspots, and accepting an issue. Their human-oriented presentation and
commands are not a runtime API for the sweep.

No caller should be folded into either of the other two callers. A shared
`nodes sonar` command would be a new product boundary, including its output
contract, credentials, and compatibility promises; that work is tracked by
Issue #218 and is not smuggled into this decision.

## Existing callers and required changes

- The machine caller builds its main-branch and per-PR issue URLs at
  `examples/pr-upkeep/sweep.py:90-99`. It already owns machine-facing queries,
  so **no change is required**.
- The `cicd` skill queries PR quality-gate, issue, and hotspot endpoints at
  `.claude/skills/cicd/scripts/pr-status.sh:123-127`. It already serves an
  operator-facing PR-status command, so **no change is required**.
- The `sonarclaude` skill declares its interactive commands at
  `.claude/skills/sonarclaude/scripts/sonar.sh:32-49` and performs API reads
  through its helper at lines 83-87. It already serves operator-facing use,
  so **no change is required**.

Thus all three current callers obey the split. Duplication of URL construction
across these different contracts is intentional at this boundary; convergence
belongs to #218 if and when that issue defines a common verb.
