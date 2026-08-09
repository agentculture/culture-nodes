# web

The Culture Nodes web front: a **read-only** Run view and Ledger view over
the `/v1alpha1` control-plane API, built with Vite + React 18 + TypeScript.

This is the PRD §8.7 first visual slice — *"Do not wait for the complete
drag-and-drop editor before proving alignment and runtime truth"*. The
graphical editor comes later; what is here is a live graph of a run, its
timeline, its ledger, and enough node detail to check what actually happened.

## What it renders

- **`/runs`** — the run list.
- **`/runs/:id`** — the Run view. A React Flow canvas laid out by ELK
  (`elkjs`, layered) from the workflow IR the run is *pinned* to (fetched by
  digest, never "latest"), overlaid with live execution state from
  `GET /v1alpha1/runs/{id}/events` (SSE, resumed by `Last-Event-ID` /
  `?from=`). A Graph ⇄ Timeline toggle switches between the canvas and the
  non-graph projection of the same information.
- **`/runs/:id/ledger`** — the run's ledger records plus any of the §10.9
  standard projections.

Node detail (contract digest, owner, kind, attempts with status and timing,
the node run's ledger delta, evidence refs) opens from a node — click, or
Enter with the node focused — into a side panel that closes on Escape and
hands focus back where it came from.

## Design layer

`src/culture-design/` is the AgentCulture visual system, pinned to the org
repo (see `docs/adr/0001-culture-design-source.md`). `tokens.css` is imported
once, globally, from `src/main.tsx`; `palette.ts` maps node kinds onto the
org's terminal palette; `edges.ts` carries the ledger-authority→line-style
mapping (`proposed` → dashed, `confirmed`/`observed`/`derived` → solid) that
this UI uses for **both** graph edges and ledger authority chips. Nothing
here invents a sibling aesthetic (PRD §8.1).

The two variable faces `tokens.css` names come from the same packages the org
site ships: `@fontsource-variable/fraunces` and
`@fontsource-variable/albert-sans`. Dark mode is `prefers-color-scheme` only —
there is no toggle upstream, and there is none here.

## Accessibility (PRD §8.8)

- Every node is a named button (`node <name>, <kind>, <state>`) in the tab
  order, in breadth-first execution order; Enter or Space opens its detail;
  arrow keys pan the canvas.
- Status is always **icon + word**, never colour alone.
- `prefers-reduced-motion: reduce` drops the active-attempt pulse and states
  the same fact as a badge instead.
- The timeline view is the non-graph alternative, carrying the same run
  information.
- Focus is visible (the `:focus-visible` convention in `tokens.css`) and
  managed across panel open/close.

## The agent-state node

The root component renders one
`<script type="application/json" id="agent-state">` mirroring what the page
is showing:

```json
{
  "status": "loading | ready",
  "route": "/runs/<id>",
  "run": { "id": "…", "state": "running", "node_states": {}, "selected": null }
}
```

`status: "ready"` means the current view finished its initial load —
including finishing it badly; a load error renders alongside, it does not
keep the page pretending to load. Assertable elements carry stable ids or
`data-` attributes (`#run-state-chip`, `#event-timeline`, `#ledger-table`,
`#node-detail-panel`, `[data-node-id]`), because webglass selectors are
`tag` / `#id` / `.class` / `[attr]` only and must match exactly one element.

## Commands

```bash
npm ci                 # install (package-lock.json is committed)
npm run dev            # vite dev server on :5173, /v1alpha1 proxied
npm run build          # tsc -b && vite build  ->  web/dist (gitignored)
npm run preview        # serve the built bundle on 127.0.0.1:4173
npm test               # vitest (components, domain, agent-state)
npm run test:e2e       # playwright, chromium — needs `npx playwright install chromium`
npm run typecheck      # tsc -b --force
```

The dev server proxies `/v1alpha1` to `http://127.0.0.1:8080`. Point it
elsewhere with `NODES_API=http://host:port npm run dev`.

The e2e suite serves the API from `e2e/fixtures/api.ts` via Playwright
request interception — the fixture in `src/fixtures/run-fixture.ts` is the
PRD §8.7 slice (intake → plan → build → test → verify, with the
`verify.changes_required` loop walked and a second build attempt in flight).
No Go server, no Postgres.

## Build integration

`npm run build` writes to `web/dist/`, which is gitignored. **The Go binary
does not embed it yet** — wiring `dist/` into the binary (an `embed.FS`
served alongside `/v1alpha1` from one origin, which is what makes the
same-origin API root in `src/api/client.ts` correct in production) is a later
task. Until then, run the UI from `npm run dev` or `npm run preview` beside
the API.

## CI

`.github/workflows/web.yml` runs on any change under `web/`:

1. **build-and-test** — `npm ci`, `npm run build`, `npm test`, then Playwright
   chromium for the keyboard walk and the reduced-motion assertion.
2. **webglass** — builds, serves `vite preview`, and drives it with
   `webglass-cli` under a scoped policy profile: `page extract --selector
   '#agent-state'` must report `status == "ready"`, and `page inspect --lens
   console` must report zero `page_errors`. Console *messages* are not
   asserted empty — no control plane runs in that job, so the run list's
   fetch legitimately 404s and the app renders its error notice rather than
   throwing.
