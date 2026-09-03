import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import { bandForZoom } from "./CultureNode";
import { DASHED, DOTTED, SOLID } from "./edges";
import { NODE_HEIGHT, NODE_WIDTH } from "../hooks/useElkLayout";

/**
 * The acceptance reference for the shared node, its edges and its canvas is
 * docs/demos/web-ui-lift/culture-nodes-lifted.html. These are CSS assertions
 * rather than rendered ones on purpose: vitest runs with `css: false`, and
 * the facts that matter here (a card's height comes from its content, an edge
 * label has no plate behind it) are declarations, not DOM.
 *
 * Everything below quotes a rule from that file, so a later edit that drifts
 * from the reference fails here rather than in somebody's eye.
 */

const nodeCss = readFileSync("src/culture-design/node.css", "utf8");
const appCss = readFileSync("src/styles/app.css", "utf8");

/** The declaration block of the first rule whose selector list matches. */
function rule(css: string, selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`(?:^|\\n)${escaped}\\s*\\{([^}]*)\\}`));
  expect(match, `no rule for ${selector}`).not.toBeNull();
  return match![1];
}

/** The value of a `--token: value;` declaration, wherever it is declared. */
function token(css: string, name: string): string {
  const match = css.match(new RegExp(`${name}\\s*:\\s*([^;]+);`));
  expect(match, `no ${name} token`).not.toBeNull();
  return match![1].trim();
}

// ---------------------------------------------------------------- point 1

describe("the card (reference `.cn`)", () => {
  const card = rule(nodeCss, ".culture-node");

  it("is 224px wide with a 64px floor and no fixed height", () => {
    expect(card).toMatch(/width:\s*224px/);
    expect(card).toMatch(/min-height:\s*64px/);
    // The defect: a 128px slab left a third of every card empty. Height is
    // content's now, at every band.
    expect(card).not.toMatch(/(?:^|[^-])height:\s*\d/);
    expect(nodeCss).not.toMatch(/\bheight:\s*128px/);
  });

  it("carries the reference's frame — radius, hairline, translucent surface, shadow", () => {
    expect(token(nodeCss, "--culture-node-radius")).toBe("0.9rem");
    expect(card).toMatch(/border-radius:\s*var\(--culture-node-radius\)/);
    expect(card).toMatch(/border:\s*1px solid var\(--terminal-line\)/);
    expect(token(nodeCss, "--terminal-line")).toBe("rgba(233, 236, 248, 0.12)");
    expect(card).toMatch(/background:\s*var\(--culture-node-surface\)/);
    expect(token(nodeCss, "--culture-node-surface")).toBe(
      "rgba(22, 27, 54, 0.92)",
    );
    expect(card).toMatch(/box-shadow:\s*var\(--culture-node-shadow\)/);
    expect(token(nodeCss, "--culture-node-shadow")).toBe(
      "0 4px 18px rgba(0, 0, 0, 0.35)",
    );
  });

  it("leaves a 2.4rem gutter for the core and its halo", () => {
    expect(card).toMatch(/padding:\s*0\.6rem 0\.75rem 0\.6rem 2\.4rem/);
  });

  it("keeps ELK's box in step: 224 wide, ~84 nominal tall", () => {
    expect(NODE_WIDTH).toBe(224);
    expect(NODE_HEIGHT).toBe(84);
  });
});

// ---------------------------------------------------------------- point 2

describe("core, halo and the one-shot ring", () => {
  it("puts a .8rem kind-coloured core in the gutter, glowing its own colour", () => {
    const core = rule(nodeCss, ".culture-node__core");
    expect(core).toMatch(/left:\s*0\.85rem/);
    expect(core).toMatch(/top:\s*0\.95rem/);
    expect(core).toMatch(/width:\s*0\.8rem/);
    expect(core).toMatch(/height:\s*0\.8rem/);
    expect(core).toMatch(/box-shadow:\s*0 0 10px var\(--k\)/);
  });

  it("sits a 2rem radial halo behind it at the resting opacity", () => {
    const halo = rule(nodeCss, ".culture-node__halo");
    expect(halo).toMatch(/width:\s*2rem/);
    expect(halo).toMatch(/height:\s*2rem/);
    expect(halo).toMatch(/radial-gradient\(circle,\s*var\(--k\)/);
    expect(halo).toMatch(/opacity:\s*0\.18/);
  });

  it("breathes only while live: .18 -> .5, scale 1 -> 1.35, 2.4s", () => {
    expect(nodeCss).toMatch(
      /\.culture-node\[data-motion="animated"\]\.is-live \.culture-node__halo \{\s*animation: culture-node-breathe 2\.4s ease-in-out infinite;/,
    );
    const breathe = nodeCss.match(
      /@keyframes culture-node-breathe \{([\s\S]*?)\n\}/,
    )?.[1];
    expect(breathe).toMatch(/opacity:\s*0\.18;\s*transform:\s*scale\(1\)/);
    expect(breathe).toMatch(/opacity:\s*0\.5;\s*transform:\s*scale\(1\.35\)/);
  });

  it("rings the CORE, not the whole card, for .9s", () => {
    const pulse = rule(nodeCss, ".culture-node__pulse");
    expect(pulse).toMatch(/width:\s*1\.8rem/);
    expect(pulse).toMatch(/height:\s*1\.8rem/);
    expect(pulse).toMatch(/border-radius:\s*50%/);
    // The old ring was `inset: -5px` — a halo around the card's whole frame.
    expect(pulse).not.toMatch(/inset:/);
    expect(nodeCss).toMatch(/animation: culture-node-ring 0\.9s ease-out/);
    const ring = nodeCss.match(/@keyframes culture-node-ring \{([\s\S]*?)\n\}/)?.[1];
    expect(ring).toMatch(/scale\(0\.6\)/);
    expect(ring).toMatch(/scale\(2\.6\)/);
  });

  it("holds the resting frame for reduced motion and data-motion=static", () => {
    // Both animations live inside the no-preference query and are gated on
    // data-motion="animated", so neither ever starts for the other two —
    // which leaves the halo at its declared .18/scale(1) resting frame.
    const animated = nodeCss.match(
      /@media \(prefers-reduced-motion: no-preference\) \{([\s\S]*?)\n\}\n\n/,
    )?.[1];
    expect(animated).toContain("culture-node-breathe");
    expect(animated).toContain("culture-node-ring");
    expect(animated!.match(/data-motion="animated"/g)).toHaveLength(2);
    expect(nodeCss).not.toMatch(/data-motion="static"\][^{]*\{[^}]*animation:/);
  });
});

// ---------------------------------------------------------------- point 3

describe("text and the state chip", () => {
  it("sets label, sub and meta to the reference's sizes and inks", () => {
    const label = rule(nodeCss, ".culture-node__label");
    expect(label).toMatch(/font-weight:\s*600/);
    // --ac-type-ui is tokens.css's 0.92rem.
    expect(label).toMatch(/font-size:\s*var\(--ac-type-ui\)/);

    const sub = rule(nodeCss, ".culture-node__sub");
    // --ac-type-meta is tokens.css's 0.75rem.
    expect(sub).toMatch(/font-size:\s*var\(--ac-type-meta\)/);
    expect(sub).toMatch(/color:\s*var\(--culture-soft\)/);

    const meta = rule(nodeCss, ".culture-node__meta");
    expect(meta).toMatch(/font-size:\s*0\.72rem/);
    expect(meta).toMatch(/font-family:\s*var\(--font-mono\)/);
    expect(meta).toMatch(/color:\s*var\(--culture-faint\)/);
  });

  it("draws the state chip as a .7rem pill in the reference's four tones", () => {
    const base = rule(
      nodeCss,
      ".culture-node .status-chip,\n.culture-node .node-card__badge,\n.culture-node .node-card__visits",
    );
    expect(base).toMatch(/font-size:\s*0\.7rem/);
    expect(base).toMatch(/border-radius:\s*999px/);

    const active = rule(
      nodeCss,
      ".culture-node .status-chip--active,\n.culture-node .status-chip--running",
    );
    expect(active).toMatch(/background:\s*var\(--culture-teal\)/);
    expect(active).toMatch(/color:\s*var\(--terminal-ground\)/);

    expect(rule(nodeCss, ".culture-node .status-chip--completed")).toMatch(
      /color:\s*var\(--culture-green\)/,
    );
    expect(
      rule(
        nodeCss,
        ".culture-node .status-chip--failed,\n.culture-node .status-chip--policy_denied,\n.culture-node .status-chip--cancelled",
      ),
    ).toMatch(/color:\s*var\(--culture-pink\)/);

    const unknown = rule(nodeCss, ".culture-node .status-chip--unknown");
    expect(unknown).toMatch(/color:\s*var\(--culture-amber\)/);
    expect(unknown).toMatch(/border-style:\s*dashed/);
  });
});

// ---------------------------------------------------------------- point 4

describe("edges", () => {
  it("rests at the reference's hairline and walks teal", () => {
    expect(token(nodeCss, "--culture-edge")).toBe("rgba(233, 236, 248, 0.28)");
    expect(DASHED.stroke).toBe("var(--culture-edge)");
    expect(DASHED.strokeWidth).toBe(1.6);
    expect(DOTTED.stroke).toBe("var(--culture-edge)");
    // Teal for a walked/active path. The width stays the org diagram's 2.4
    // (scripts/check-culture-design.mjs pins it), not the demo's 2.2.
    expect(SOLID.stroke).toBe("var(--culture-teal)");
  });

  it("tips every arrow with the reference's marker ink", () => {
    expect(token(nodeCss, "--culture-edge-marker")).toBe(
      "rgba(233, 236, 248, 0.5)",
    );
    const marker = rule(
      appCss,
      ".react-flow__arrowhead polyline,\n.react-flow__arrowhead path",
    );
    expect(marker).toMatch(/fill:\s*var\(--culture-edge-marker\)/);
  });

  it("dashes a loop", () => {
    expect(rule(appCss, ".flow-edge.is-loop .react-flow__edge-path")).toMatch(
      /stroke-dasharray:\s*5 5/,
    );
  });

  it("labels edges in 10px mono with NO plate behind them", () => {
    const text = rule(appCss, ".react-flow__edge-text");
    expect(text).toMatch(/font-family:\s*var\(--font-mono\)/);
    expect(text).toMatch(/font-size:\s*10px/);
    expect(text).toMatch(/fill:\s*var\(--culture-faint\)/);
    // The white box was the loudest thing on the canvas.
    expect(rule(appCss, ".react-flow__edge-textbg")).toMatch(
      /fill:\s*transparent/,
    );
  });

  it("tints the three mesh relations blue, yellow and amber", () => {
    expect(token(nodeCss, "--culture-edge-machine")).toBe(
      "rgba(127, 179, 242, 0.45)",
    );
    expect(token(nodeCss, "--culture-edge-workflow")).toBe(
      "rgba(230, 205, 122, 0.45)",
    );
    expect(token(nodeCss, "--culture-edge-run")).toBe("rgba(242, 183, 116, 0.6)");
    expect(
      rule(appCss, ".mesh-edge--actor-machine .react-flow__edge-path"),
    ).toMatch(/stroke:\s*var\(--culture-edge-machine\)/);
    expect(
      rule(appCss, ".mesh-edge--actor-workflow .react-flow__edge-path"),
    ).toMatch(/stroke:\s*var\(--culture-edge-workflow\)/);
    expect(
      rule(
        appCss,
        ".mesh-edge--run-actor .react-flow__edge-path,\n.mesh-edge--run-workflow .react-flow__edge-path",
      ),
    ).toMatch(/stroke:\s*var\(--culture-edge-run\)/);
  });

  it("uses React Flow's default curve, not a right-angled smoothstep", () => {
    for (const file of [
      "src/components/ActiveGraphCanvas.tsx",
      "src/routes/Mesh.tsx",
    ]) {
      expect(readFileSync(file, "utf8"), file).not.toMatch(
        /type:\s*"smoothstep"/,
      );
    }
  });
});

// ---------------------------------------------------------------- point 5

describe("the canvas", () => {
  it("keeps the terminal ground and paints the 26px dotted grid on it", () => {
    const surface = rule(appCss, ".canvas-surface.canvas-surface");
    expect(surface).toMatch(/background-color:\s*var\(--terminal-ground\)/);
    expect(surface).toMatch(
      /background-image:\s*radial-gradient\(\s*var\(--culture-canvas-grid\)/,
    );
    expect(surface).toMatch(/background-size:\s*26px 26px/);
    expect(token(nodeCss, "--culture-canvas-grid")).toBe(
      "rgba(233, 236, 248, 0.08)",
    );
    // One grid, not two: React Flow's own pattern would sit under it at a
    // different spacing and pan with the viewport.
    expect(rule(appCss, ".canvas-surface .react-flow__background")).toMatch(
      /display:\s*none/,
    );
  });

  it("leaves the zoom bands where they were", () => {
    expect(bandForZoom(0.3)).toBe("far");
    expect(bandForZoom(0.8)).toBe("medium");
    expect(bandForZoom(1.4)).toBe("close");
    // The far band only gets a shorter floor; what each band *renders* is
    // CultureNode.tsx's decision and is unchanged.
    const far = rule(nodeCss, '.culture-node[data-band="far"]');
    expect(far).toMatch(/min-height:\s*44px/);
    expect(nodeCss).not.toMatch(/data-band="(?:medium|close)"\]\s*\{/);
  });
});
