# Design canvas YAML round-trip spike

Date: 2026-09-03  
Status: spike evidence; operator verdict: go (2026-09-03)

## Time box and isolation

The experiment ran from 2026-09-03T18:37:45Z to
2026-09-03T18:39:44Z (1 minute 59 seconds elapsed). It used the installed
`yaml` 2.9.0 package from `web/node_modules` in disposable Node processes.
The only temporary output was `/tmp/t12-mutated.workflow.yaml`; no prototype
code was added under `web/src` and the decision record is the only repository
deliverable. This is the throwaway-worktree boundary requested by the spike;
the dispatch explicitly substituted the already isolated checkout and `/tmp`
for creating another Git worktree.

## No-edit round-trip test

Each test parsed the original bytes with `parseDocument(source)`, called
`doc.toString(options)`, and compared the result to `source` with strict string
equality. The tested inputs were:

- `deploy/compose/testdata/smoke.workflow.yaml` (2,025 bytes, 82 content lines);
- `VALID_YAML_SOURCE` in `web/src/fixtures/authoring-fixture.ts` (369 bytes);
- `INVALID_YAML_SOURCE` in that same file (202 bytes).

The smallest exact option set that passed byte-for-byte on all three inputs
was:

```ts
doc.toString({ flowCollectionPadding: false })
```

The explicit editor baseline also passed on all three inputs:

```ts
doc.toString({
  flowCollectionPadding: false,
  lineWidth: 0,
  indentSeq: true,
})
```

`lineWidth: 0` prevents future long scalar folding and `indentSeq: true`
states the fixture's sequence indentation rather than depending on the
library default. There are no residual difference classes for the three
bound inputs with either passing option set.

Control and option-search results:

| Options | Smoke | Valid frame fixture | Invalid frame fixture | Difference class |
| --- | --- | --- | --- | --- |
| `{}` | fail, 4 lines | fail, 1 line | pass | spaces inserted inside flow collections |
| `{ flowCollectionPadding: false }` | pass | pass | pass | none |
| `{ flowCollectionPadding: false, lineWidth: 0 }` | pass | pass | pass | none |
| baseline above | pass | pass | pass | none |
| baseline with `indentSeq: false` | fail, 2 lines | fail, 2 lines | fail, 2 lines | block-sequence dash and continuation dedented by two spaces |

This test covers only the committed fixtures named above. It is not a general
claim that arbitrary author-written YAML is lossless; YAML constructs and
styles absent from these fixtures still require golden coverage before an
editor ships.

## One-node, one-edge mutation test

Starting from the smoke fixture, the disposable script performed exactly
these two document mutations before stringifying with the explicit baseline:

```ts
doc.addIn(["spec", "nodes", "recover"], {
  kind: "end",
  ownerRef: "team/platform-ai",
  output: { from: "/nodes/work/output" },
})
doc.addIn(["spec", "edges"], { from: "work.failed", to: "recover" })
```

The resulting unified diff was:

```diff
@@ -75,7 +75,14 @@
       ownerRef: team/platform-ai
       output:
         from: /nodes/work/output
+    recover:
+      kind: end
+      ownerRef: team/platform-ai
+      output:
+        from: /nodes/work/output
 
   edges:
     - from: work.completed
       to: finish
+    - from: work.failed
+      to: recover
```

Thus the mutation changed only the inserted node and edge; existing text,
comments, blank lines, key order, and the existing edge remained byte-identical.

## Validation POST test

The exact mutated string was sent as
`{"format":"yaml","source":"<exact mutated document>"}` to
`POST http://thor:18080/v1alpha1/workflows/validate`. The reachable production
control plane returned:

```text
HTTP 401
{"code":1,"message":"request refused","reason":"no_principal","remediation":"authenticate with a bound principal holding the required role"}
```

Repeating the request with the session's bridge bearer produced the same
result. No bound principal credential was available to this sandbox, and the
Go toolchain required to run the compiler locally was also absent. Therefore
this session did **not** obtain `valid: true`; server-side compile validity of
the mutated result remains an explicit merge-gate check. The POST boundary was
tested and failed closed without exposing a secret.

Operator go/no-go: ______________________________

## Operator addendum at the merge gate (2026-09-03)

The validate step the sandbox could not finish was rerun on spark with the
offline compiler the CLI ships (`go run ./cmd/nodes validate`): the same two
mutations were replayed with the explicit editor baseline options on
`deploy/compose/testdata/smoke.workflow.yaml` (no-edit round-trip byte-identical
again), and the mutated document was compiled locally. Result, verbatim:

```text
valid: compose-smoke 1.0.0 (0 errors, 0 warnings)
digest: sha256:a561b0deed31133824b132f6da9a87688f33f5f2466fd9bb76ee5e23f6f466eb
```

Exit code `0`. This is the compiler itself, not the HTTP route, so it
proves the document compiles; the route adds only the principal check the
sandbox lacked (issue #279).

## Go / no-go

Operator decision recorded 2026-09-03 on the evidence above; the editor wave
(t14, t15) proceeds.

- Verdict: **go** (operator, 2026-09-03)
