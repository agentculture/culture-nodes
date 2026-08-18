# combining-loop — issue #118's step 7, as a graph

The operator's merge loop, per finished work package: **harvest** the
handed-over ref (saved under `refs/culture-nodes/harvested/<run-id>` before
anything else touches it) → **stage** a detached candidate of the feature
branch → **gate the combination** with the unchanged `merge-gate.py` →
**merge and push** on a `gates_passed` verdict for that exact candidate →
surface the agent's **claim for a human decision** → **release** whatever
the package unlocks.

Start it with one durable event (hand-emitted in the stage-1 demo):

```bash
curl -s -X POST "$NODES_API_URL/v1alpha1/events" \
  -H "Authorization: Bearer $NODES_EVENT_INGRESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "combining-loop.package.finished",
        "subject": "<package-id>",
        "payload": {
          "package_id": "<package-id>",
          "handover_ref": "refs/culture-nodes/<run>/<node-run>",
          "expected_commit": "<sha>",
          "feature_branch": "spec/<feature>",
          "unlocks": []
        }
      }'
```

The `subject` is what keeps one package to one run (t15): a second event
for a package mid-flight attaches to the existing run.

The code nodes run `scripts/combining-loop-node.py`, fetched by pinned
digest exactly as merge-gate fetches its program; per-run values reach each
process as `NODES_INPUT_JSON` (#170's fix, deviation d4). The workflow
header documents the deployment grants, why domain answers ride output JSON
instead of custom exit codes, and the three deliberate non-behaviors
(repair routing stays control-plane-side; no machine merge over an
unmeasured combination; the claim decision records judgement without
blocking release).
