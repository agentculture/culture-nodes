# Backlog after-state queries

These four queries reproduce the cycle's closing counts. They intentionally
emit JSON so another reader can count or inspect the exact issue set.

```bash
gh issue list --repo agentculture/culture-nodes --state open --limit 1000 --json number
gh issue list --repo agentculture/culture-nodes --state closed --search 'closed:>=2026-08-15' --limit 1000 --json number,closedAt
gh issue list --repo agentculture/culture-nodes --state all --search 'created:>=2026-08-15' --limit 1000 --json number,createdAt,state
gh issue list --repo agentculture/culture-nodes --state open --search 'created:>=2026-08-15' --limit 1000 --json number,createdAt
```
