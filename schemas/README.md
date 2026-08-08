# schemas

JSON Schema **Draft 2020-12** contract definitions, embedded into the control
plane by `embed.go` and validated by `internal/contracts`. JSON is canonical;
YAML is authoring sugar the compiler accepts, not a second source of truth.

Every schema's `$id` is `https://nodes.culture.dev/schemas/` plus its path here,
so cross-file `$ref`s resolve offline. That correspondence is load-bearing:
`internal/contracts` registers each embedded file under exactly that URL.

| Path | What it defines |
| --- | --- |
| `ledger/envelope.schema.json` | The common record envelope and the authority enum (PRD §10.3, §10.4) |
| `ledger/<type>.schema.json` | One file per MVP record type (PRD §10.2); each `$ref`s the envelope |
| `ledger/record.schema.json` | Any record — envelope plus `if`/`then` dispatch on `record_type` |
| `workflow/workflow.schema.json` | The workflow authoring document (PRD §9.1, §11.1) |
| `runner/operation.schema.json` | A typed code-execution operation (PRD §13.7) |
| `runner/result.schema.json` | A runner result with per-observation completeness (PRD §13.7) |
| `examples/` | Reference documents, all validated by the test suite |

## What these schemas do not decide

Validation here is the syntax and structure level of PRD §11.4. Graph
reachability, contract compatibility, binding resolution, digest pinning, and
policy belong to the compiler; runtime validation stays mandatory regardless,
because JSON Schema is a validation language and not a decidable subtyping
system.

Schema validity is also not authority. Whether a producer may write a record
with a given `authority` is enforced by the ledger runtime against an
authenticated identity — no document can prove that about itself.

## Known gaps

- `pre_run` / `post_run` on a node are **declared but unvalidated** stubs. The
  hook contract is specified by a later task; until then the keywords accept
  any value and carry a `$comment` saying so. Acceptance there is not a
  contract.
- Record `data` payloads are loose. Named properties are optional and
  documentary, taken from the PRD's own anatomy for tasks, evidence, and
  reviews; no payload is pinned yet.
- Runner payload shapes are runner-neutral by construction, so a platform limit
  (a duration cap, a payload size) is an adapter-side rejection, not a schema
  constraint.
