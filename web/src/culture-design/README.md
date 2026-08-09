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

This directory is compiled. The web app at `web/` (Vite + React 18 +
TypeScript) imports every file here, so `mark.tsx` is type-checked by
`tsc -b` and bundled by `vite build`; its earlier placeholder `FC` shim is
gone in favour of React's own type. `tokens.css` is imported once, globally,
from `src/main.tsx`.

## Verification

`scripts/check-culture-design.mjs` (repo root) verifies this layer stays
faithful to its pinned source: `tokens.css` byte-matches the pinned org
file, every hex in `palette.ts` traces back to an org source file, and
`edges.ts`'s dasharray values match the org diagram conventions. Run it
with:

```bash
node scripts/check-culture-design.mjs
```
