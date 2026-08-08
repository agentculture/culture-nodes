# ADR 0001: culture-design layer sourced from agentculture/org, pinned commit

- **Status:** accepted
- **Date:** 2026-08-08
- **Task:** t5 (culture-nodes app design plan,
  `.devague/plans/culture-nodes-app-design.json`)

## Context

Culture Nodes' web UI (PRD §18, `docs/initial-design/culture-nodes-prd-spec.md`)
needs to carry agentculture.org's design system rather than invent its own —
the org site is the canonical visual identity for everything in the
AgentCulture family, including this product. The org site
(`/home/spark/git/org`, an Astro project at `site-astro/`) is developed
independently of culture-nodes and has no versioned package we can depend
on; it is a sibling repo on this machine, checked out read-only for this
purpose.

Rather than take a live dependency on a moving sibling repo, this ADR
records a **point-in-time extraction**: specific files, copied (or ported)
at one pinned commit, checked into this repo under
`web/src/culture-design/`. The org repo is never modified by culture-nodes
work, and culture-nodes never imports from `/home/spark/git/org` at build
or run time — everything culture-nodes needs is copied here.

## Pinned commit

```text
org repo:  /home/spark/git/org  (agentculture/org)
pin:       b4d939ba0aa354a5ae53065319a773e0013de698
```

Obtained with:

```bash
git -C /home/spark/git/org rev-parse HEAD
```

at the time task t5 was executed (2026-08-08).

## What was extracted

| File | Extracted from (at the pin) | What it carries |
| --- | --- | --- |
| `web/src/culture-design/tokens.css` | `site-astro/src/styles/global.css` | The full design-token contract, copied **verbatim** below a header comment: color tokens (light + `@media (prefers-color-scheme: dark)` overrides), the mesh palette (`--mesh-node`, `--mesh-thread`, `--mesh-halo*`), type scale/leading/tracking roles, layout rail widths, motion easings, and the reduced-motion kill switch. Framework-agnostic CSS custom properties — no Astro-specific syntax. |
| `web/src/culture-design/mark.tsx` | `site-astro/src/components/Mark.astro` | The AgentCulture mark ("three agents, two threads"), ported from an Astro single-file component to a typed React SVG component. Geometry (viewBox, path, circle positions/radii) copied verbatim; colors kept as `var(--mesh-node)` / `var(--mesh-thread)` references into `tokens.css` so the mark stays theme-aware with no JS. |
| `web/src/culture-design/palette.ts` | `site-astro/src/components/ColleagueTerminal.astro` (`.accent-*` / `.dot` / `.accent-tag` rules, and the `.term` ground rules) | The terminal 7-color categorical palette (teal/blue/amber/violet/pink/green/yellow + neutral fallback), re-mapped from org's per-capture-category accent onto Culture Nodes' node kinds (agent/code/decision/approval/wait/end/action.http/subworkflow), plus the fixed dark terminal ground colors (`#10142b` background, `#e9ecf8`/`#a9b0cf`/`#8790b8`/`#c7cde8` text tones). |
| `web/src/culture-design/edges.ts` | `site-astro/src/components/CliRuntimeStackDiagram.astro` (`.connector.solid` / `.connector.dotted` / `.layer-box.replaceable` rules and figcaption) | The sitewide edge/diagram semantic — solid = confirmed/operational path, dashed (`9 7`) = optional/replaceable/not-yet-fixed, dotted (`2 7`) = a soft reference path outside the main authority chain — re-encoded as `SOLID` / `DASHED` / `DOTTED` constants and mapped onto culture-nodes' ledger authority states (`proposed` -> DASHED, `confirmed`/`observed`/`derived` -> SOLID). |

Nothing else from the org repo was copied. In particular, no fonts, no
JavaScript/hydration behavior, no Astro components other than the two
named above, and no page content.

## Re-pin procedure

When the org design system changes in a way culture-nodes should pick up:

1. `git -C /home/spark/git/org rev-parse HEAD` to get the new commit.
2. Re-copy `site-astro/src/styles/global.css` into
   `web/src/culture-design/tokens.css`, keeping the header comment but
   updating its `Pinned commit:` line to the new hash. The copied body
   below the header must stay byte-identical to the org source — do not
   hand-edit it.
3. Re-diff `site-astro/src/components/Mark.astro` against
   `web/src/culture-design/mark.tsx` and port any geometry/color changes by
   hand (it is a port, not a verbatim copy, so this step is manual).
4. Re-diff the accent rules in `ColleagueTerminal.astro` against
   `web/src/culture-design/palette.ts` and the connector rules in
   `CliRuntimeStackDiagram.astro` against `web/src/culture-design/edges.ts`;
   update hex values / dasharray values if they moved.
5. Update the pinned commit hash in this ADR (the table above and the
   "Pinned commit" section).
6. Run `node scripts/check-culture-design.mjs` — it re-derives the pin from
   this ADR, re-fetches the org source at that pin via
   `git -C /home/spark/git/org show <pin>:<path>`, and fails loudly if
   `tokens.css` no longer byte-matches, if a `palette.ts` hex no longer
   traces back to an org source file, or if `edges.ts`'s dasharray values
   no longer match the org diagram conventions.

The script never mutates the org checkout and never assumes org's working
tree — it always reads through `git show <pin>:<path>`, so it verifies
against the exact pinned revision even if `/home/spark/git/org`'s HEAD has
since moved on.

## License note

The org repo (`/home/spark/git/org`) is licensed Apache License 2.0 (see
its `LICENSE` file). Apache-2.0 grants a copyright license broad enough to
cover copying and adapting these source files into culture-nodes (also
Apache-2.0, per this repo's `LICENSE`), including the required
verbatim-notice handling for `tokens.css`.

**Apache-2.0 does not grant any trademark license** (License §6,
"Trademarks"). "AgentCulture" as a name/brand and the mark's specific
graphic identity are not licensed for use as a trademark by this
extraction — this ADR covers reuse of the *code* (CSS custom properties,
SVG geometry, color values, diagram conventions), not permission to
represent culture-nodes as an official AgentCulture product or to reuse
the mark as a trademark outside this shared design system.

## Dark mode

`tokens.css`'s dark values live entirely under
`@media (prefers-color-scheme: dark)` — there is no light/dark toggle
anywhere in the org site, and none is introduced here. culture-nodes'
consumption of `tokens.css` follows the same rule: dark mode is derived
purely from the visitor's OS/browser preference, never from an
application-level toggle, unless a future ADR explicitly revisits this.

## Consequences

- `web/src/culture-design/` has no live dependency on `/home/spark/git/org`
  at build or run time; it is fully self-contained once copied.
- Drift between org and culture-nodes' copy is possible and expected over
  time; `scripts/check-culture-design.mjs` catches only drift in
  `tokens.css`/`palette.ts`/`edges.ts` against the *recorded* pin — it does
  not warn when org's HEAD moves further. Picking up new upstream design
  changes is a deliberate, manual re-pin (see above), not automatic.
- `mark.tsx` is not yet compiled or type-checked (no `node_modules` in this
  repo yet) — see `web/src/culture-design/README.md`. That lands with the
  web app / Vite build-out task.
