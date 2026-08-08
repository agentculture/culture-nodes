# culture-design

Culture Nodes' web UI design layer, extracted from agentculture/org's site
design system at a pinned commit. See
`docs/adr/0001-culture-design-source.md` at the repo root for the pin, the
extraction rationale, the license note, and the re-pin procedure.

## Contents

- `tokens.css` — `global.css` copied verbatim from the org repo (framework-
  agnostic CSS custom properties). Do not hand-edit the copied section; see
  the ADR to re-pin.
- `mark.tsx` — the AgentCulture mark (`Mark.astro`), ported to a typed React
  SVG component.
- `palette.ts` — the terminal 7-color categorical palette, mapped to
  Culture Nodes' node kinds, plus the fixed terminal ground colors.
- `edges.ts` — edge-style constants (solid/dashed/dotted) encoding the
  ledger-authority-to-visual-style mapping used across the workflow canvas.

## Compilation status

This directory currently has **no build tooling** — there is no
`node_modules`, no `package.json`, no TypeScript/React toolchain wired up
in this repo yet. `mark.tsx` is syntactically valid TSX but is **not**
type-checked or compiled as part of this task. Compilation lands with the
web app / Vite build-out task, which will add the dependency tree
(`react`, `@types/react`, a bundler config) that this file is written to
be consumed by.

`tokens.css`, `palette.ts`, and `edges.ts` are plain CSS/TS with no
framework dependency and can already be consumed by any bundler once one
exists.

## Verification

`scripts/check-culture-design.mjs` (repo root) verifies this layer stays
faithful to its pinned source: `tokens.css` byte-matches the pinned org
file, every hex in `palette.ts` traces back to an org source file, and
`edges.ts`'s dasharray values match the org diagram conventions. Run it
with:

```bash
node scripts/check-culture-design.mjs
```
