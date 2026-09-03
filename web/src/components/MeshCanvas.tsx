import { useEffect, useRef, useState } from "react";
import { TERMINAL_GROUND } from "../culture-design/palette";
import {
  ACTOR_GLYPH_STYLE,
  CONTROL_PLANE_ID,
  RUN_COLOR,
  RUN_FLARE_COLOR,
  RUN_SETTLE_COLOR,
  hash01,
  layoutMesh,
} from "../domain/mesh";
import type { MeshEventAction, MeshGraph } from "../domain/mesh";
import CategoryChip from "./CategoryChip";

/**
 * MeshCanvas — the live-mesh's Canvas-2D renderer (task t18), porting the
 * craft idioms of culture.dev's MeshIsland.svelte (the reference component
 * named in the plan) to React:
 *
 *   - pre-rendered glow SPRITES blitted per node — never a radial gradient
 *     built inside the frame loop;
 *   - quantized-alpha style TABLES built once per color — no rgba string
 *     concatenation in the hot loop (t18's "no per-frame allocations");
 *   - a fixed-size particle POOL — a burst of committed events reuses slots
 *     and, at the cap, coalesces into an edge-glow surge instead of
 *     allocating;
 *   - deterministic layout (domain/mesh.ts) recomputed — never
 *     re-randomized — on resize, via ResizeObserver + DPR scaling;
 *   - IntersectionObserver + visibilitychange pause the loop the moment
 *     the canvas is offscreen or the tab is hidden;
 *   - prefers-reduced-motion renders exactly ONE dignified static frame
 *     and never starts the loop — the reference's exact discipline.
 *
 * The ground is TERMINAL_GROUND — the org's fixed dark terminal backdrop
 * that "never changes with the theme" (culture-design/palette.ts) — so the
 * terminal palette's node colors keep their contrast in the light theme
 * too: the mesh is a viewport into the org's nervous system, not a page
 * surface.
 *
 * Everything drawn corresponds to committed API state: nodes/edges from
 * the assembled graph prop, particles one-to-one with committed events
 * delivered through `bus` (honesty condition h14 — no canned data, no
 * decorative traffic).
 */

/** The route -> canvas action channel (a ref-stable single-listener bus). */
export interface MeshActionBus {
  listener: ((action: MeshEventAction) => void) | null;
}

export interface MeshCanvasProps {
  graph: MeshGraph;
  reducedMotion: boolean;
  bus: MeshActionBus;
}

interface SimNode {
  id: string;
  kind: "center" | "actor" | "run";
  label: string;
  sub: string;
  category: string | null;
  color: string;
  dim: number;
  square: boolean;
  r: number;
  /** Base (layout) position; dx/dy are the per-frame drawn position. */
  bx: number;
  by: number;
  z: number;
  dx: number;
  dy: number;
  phase: number;
  driftSpeed: number;
  glowPhase: number;
  /** Edge anchor node id ("" for the center itself). */
  anchorId: string;
  edgePhase: number;
  /** Entrance choreography: the loop-time this node settles in at. */
  appearAt: number;
  /** Lifecycle resolution animation, once a terminal event arrives. */
  resolved: { outcome: "completed" | "failed" | "cancelled"; at: number } | null;
  /** Coalesced-burst edge surge (decays each frame). */
  surge: number;
}

interface Particle {
  active: boolean;
  fromId: string;
  toId: string;
  p: number;
  speed: number;
  color: string;
}

const MAX_PARTICLES = 64;
/**
 * Deterministic background star-dust: pure ground texture for depth — far
 * dimmer and smaller than any data node (max alpha 0.09, r <= 1.3px, no
 * glow, no edges), so it can never be mistaken for one. Positions are
 * hash-seeded fractions of the canvas, stable across resizes.
 */
const DUST_COUNT = 110;
const ENTRANCE_STAGGER = 0.045; // s between node arrivals (~600ms total)
const ENTRANCE_DURATION = 0.6;
const DRIFT_AMP = 5; // px of idle float, scaled by each node's depth
const ALPHA_STEPS = 32;

const EDGE_RGB = "233, 236, 248"; // starlight ink on the terminal ground

function easeOutCubic(t: number): number {
  const u = 1 - t;
  return 1 - u * u * u;
}

function lerp(a: number, b: number, t: number): number {
  return a + (b - a) * t;
}

function hexToRgb(hex: string): string {
  const n = parseInt(hex.slice(1), 16);
  return `${(n >> 16) & 0xff}, ${(n >> 8) & 0xff}, ${n & 0xff}`;
}

/** Quantized-alpha stroke/fill styles, built once per color. */
function makeStyleTable(rgb: string): string[] {
  const out = new Array<string>(ALPHA_STEPS + 1);
  for (let i = 0; i <= ALPHA_STEPS; i++) {
    out[i] = `rgba(${rgb}, ${(i / ALPHA_STEPS).toFixed(3)})`;
  }
  return out;
}

function styleAt(table: string[], alpha: number): string {
  const i = Math.max(0, Math.min(ALPHA_STEPS, Math.round(alpha * ALPHA_STEPS)));
  return table[i];
}

/** Pre-render one radial glow sprite for a color (blitted, never rebuilt). */
function makeGlowSprite(rgb: string): HTMLCanvasElement | null {
  const size = 64;
  const sprite = document.createElement("canvas");
  sprite.width = sprite.height = size;
  const g = sprite.getContext("2d");
  if (!g) return null;
  const grad = g.createRadialGradient(size / 2, size / 2, 0, size / 2, size / 2, size / 2);
  grad.addColorStop(0, `rgba(${rgb}, 1)`);
  grad.addColorStop(0.55, `rgba(${rgb}, 0.28)`);
  grad.addColorStop(1, `rgba(${rgb}, 0)`);
  g.fillStyle = grad;
  g.fillRect(0, 0, size, size);
  return sprite;
}

/** The AgentCulture mark's threads (culture-design/mark.tsx geometry, 64-box). */
const MARK_PATH = "M14 46 Q 24 38 33 21 M33 21 Q 40 36 51 41";
const MARK_NODES: Array<[number, number, number]> = [
  [14, 46, 5],
  [33, 21, 6.5],
  [51, 41, 4.5],
];

export function MeshCanvas({ graph, reducedMotion, bus }: MeshCanvasProps) {
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const tooltipRef = useRef<HTMLDivElement | null>(null);
  const graphRef = useRef(graph);
  const [inspected, setInspected] = useState<SimNode | null>(null);
  const inspectedIdRef = useRef<string | null>(null);

  // All simulation state lives in one ref so the rAF loop never touches
  // React state (no per-frame renders, no per-frame closures).
  const simRef = useRef<{
    nodes: SimNode[];
    byId: Map<string, SimNode>;
    particles: Particle[];
    sprites: Map<string, HTMLCanvasElement | null>;
    tables: Map<string, string[]>;
    edgeTable: string[];
    ctx: CanvasRenderingContext2D | null;
    W: number;
    H: number;
    /** Node-size scale for the canvas size (1 on small, up on wide). */
    scale: number;
    raf: number;
    running: boolean;
    visible: boolean;
    t0: number;
    lastT: number | undefined;
    entered: boolean;
    nextAppear: number;
    parallaxX: number;
    parallaxY: number;
    targetParallaxX: number;
    targetParallaxY: number;
    markPath: Path2D | null;
    dust: Array<{ fx: number; fy: number; phase: number; depth: number }>;
  } | null>(null);

  function sim() {
    if (!simRef.current) {
      const particles: Particle[] = [];
      for (let i = 0; i < MAX_PARTICLES; i++) {
        particles.push({ active: false, fromId: "", toId: "", p: 0, speed: 0, color: RUN_COLOR });
      }
      simRef.current = {
        nodes: [],
        byId: new Map(),
        particles,
        sprites: new Map(),
        tables: new Map(),
        edgeTable: makeStyleTable(EDGE_RGB),
        ctx: null,
        W: 800,
        H: 460,
        scale: 1,
        raf: 0,
        running: false,
        visible: true,
        t0: 0,
        lastT: undefined,
        entered: false,
        nextAppear: 0,
        parallaxX: 0,
        parallaxY: 0,
        targetParallaxX: 0,
        targetParallaxY: 0,
        markPath: typeof Path2D === "undefined" ? null : new Path2D(MARK_PATH),
        dust: Array.from({ length: DUST_COUNT }, (_, i) => ({
          fx: hash01(`dust-x-${i}`),
          fy: hash01(`dust-y-${i}`),
          phase: hash01(`dust-p-${i}`) * Math.PI * 2,
          depth: 0.5 + hash01(`dust-z-${i}`) * 0.5,
        })),
      };
    }
    return simRef.current;
  }

  function tableFor(color: string): string[] {
    const s = sim();
    let table = s.tables.get(color);
    if (!table) {
      table = makeStyleTable(hexToRgb(color));
      s.tables.set(color, table);
    }
    return table;
  }

  function spriteFor(color: string): HTMLCanvasElement | null {
    const s = sim();
    if (!s.sprites.has(color)) {
      s.sprites.set(color, makeGlowSprite(hexToRgb(color)));
    }
    return s.sprites.get(color) ?? null;
  }

  /** Current loop time in seconds (0 while the loop has not started). */
  function loopTime(): number {
    const s = sim();
    if (!s.running || typeof performance === "undefined") return s.lastT ?? 0;
    return (performance.now() - s.t0) / 1000;
  }

  /** Reconcile the graph prop into sim nodes, keeping per-id motion state. */
  function reconcile() {
    const s = sim();
    const g = graphRef.current;
    const now = loopTime();
    const prior = s.byId;
    const nodes: SimNode[] = [];
    const byId = new Map<string, SimNode>();
    const positions = layoutMesh(g, s.W, s.H);

    const firstBuild = !s.entered;
    let appear = firstBuild ? 0.1 : Math.max(now + 0.05, s.nextAppear);

    const upsert = (
      id: string,
      make: () => Omit<
        SimNode,
        "bx" | "by" | "z" | "dx" | "dy" | "phase" | "driftSpeed" | "glowPhase" | "edgePhase" | "appearAt" | "resolved" | "surge"
      >,
    ) => {
      const pos = positions.get(id) ?? { x: s.W / 2, y: s.H / 2, z: 1 };
      const existing = prior.get(id);
      if (existing) {
        existing.bx = pos.x;
        existing.by = pos.y;
        existing.z = pos.z;
        const next = make();
        existing.label = next.label;
        existing.sub = next.sub;
        existing.category = next.category;
        existing.anchorId = next.anchorId;
        existing.r = next.r;
        nodes.push(existing);
        byId.set(id, existing);
        return;
      }
      const appearAt = appear;
      appear += ENTRANCE_STAGGER;
      const node: SimNode = {
        ...make(),
        bx: pos.x,
        by: pos.y,
        z: pos.z,
        dx: pos.x,
        dy: pos.y,
        phase: hash01(id, 3) * Math.PI * 2,
        driftSpeed: 0.5 + hash01(id, 4) * 0.5,
        glowPhase: hash01(id, 5) * Math.PI * 2,
        edgePhase: hash01(id, 6) * Math.PI * 2,
        appearAt,
        resolved: null,
        surge: 0,
      };
      nodes.push(node);
      byId.set(id, node);
    };

    upsert(CONTROL_PLANE_ID, () => ({
      id: CONTROL_PLANE_ID,
      kind: "center",
      label: "control plane",
      sub: "culture-nodes",
      category: null,
      color: TERMINAL_GROUND.ink,
      dim: 1,
      square: false,
      r: 21,
      anchorId: "",
    }));

    for (const actor of g.actors) {
      const style = ACTOR_GLYPH_STYLE[actor.glyph];
      upsert(actor.id, () => ({
        id: actor.id,
        kind: "actor",
        label: actor.actorKey,
        sub: actor.kind,
        category: null,
        color: style.color,
        dim: style.dim,
        square: style.shape === "square",
        r: (actor.glyph === "human" ? 10 : 12) * s.scale,
        anchorId: CONTROL_PLANE_ID,
      }));
    }

    for (const run of g.runs) {
      upsert(run.id, () => ({
        id: run.id,
        kind: "run",
        label: run.label,
        sub: `run · ${run.state}`,
        category: run.category,
        color: RUN_COLOR,
        dim: 0.9,
        square: false,
        r: 6.5 * s.scale,
        anchorId: run.attachedTo,
      }));
    }

    // A node leaving the graph simply stops being drawn — the route holds
    // resolved runs in the graph through their settle/fade animation first.
    s.nodes = nodes;
    s.byId = byId;
    s.nextAppear = appear;
    s.entered = true;

    // Warm the sprite/style caches outside the frame loop.
    for (const node of nodes) {
      spriteFor(node.color);
      tableFor(node.color);
    }
    spriteFor(RUN_SETTLE_COLOR);
    tableFor(RUN_SETTLE_COLOR);
    spriteFor(RUN_FLARE_COLOR);
    tableFor(RUN_FLARE_COLOR);
  }

  function spawnParticle(fromId: string, toId: string, color: string) {
    const s = sim();
    for (let i = 0; i < s.particles.length; i++) {
      const part = s.particles[i];
      if (!part.active) {
        part.active = true;
        part.fromId = fromId;
        part.toId = toId;
        part.p = 0;
        part.speed = 0.55 + hash01(fromId + toId, i) * 0.35;
        part.color = color;
        return;
      }
    }
    // Pool exhausted: coalesce the burst into an edge surge instead.
    const node = s.byId.get(fromId) ?? s.byId.get(toId);
    if (node) node.surge = Math.min(node.surge + 0.5, 2);
  }

  function handleAction(action: MeshEventAction) {
    const s = sim();
    if (action.kind === "none") return;
    if (action.kind === "pulse") {
      const run = s.byId.get(action.runId);
      if (!run) return; // an event for a run we don't display: no invented pulse
      const anchor = run.anchorId || CONTROL_PLANE_ID;
      if (action.direction === "outbound") spawnParticle(anchor, run.id, run.color);
      else spawnParticle(run.id, anchor, run.color);
      run.surge = Math.min(run.surge + 0.25, 2);
      if (reducedMotion) renderStatic();
      return;
    }
    if (action.kind === "run-resolved") {
      const run = s.byId.get(action.runId);
      if (!run) return;
      run.resolved = { outcome: action.outcome, at: loopTime() };
      const anchor = run.anchorId || CONTROL_PLANE_ID;
      spawnParticle(
        run.id,
        anchor,
        action.outcome === "completed" ? RUN_SETTLE_COLOR : RUN_FLARE_COLOR,
      );
      if (reducedMotion) renderStatic();
    }
    // "run-added" arrives as a graph prop change; the reconcile gives the
    // new node its own entrance.
  }

  function resize() {
    const s = sim();
    const canvas = canvasRef.current;
    const wrap = wrapRef.current;
    if (!canvas || !wrap) return;
    const cssW = Math.max(320, Math.round(canvas.clientWidth || wrap.clientWidth || 320));
    s.W = cssW;
    s.H = Math.min(640, Math.max(360, Math.round(cssW * 0.5)));
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    canvas.width = Math.round(s.W * dpr);
    canvas.height = Math.round(s.H * dpr);
    canvas.style.height = `${s.H}px`;
    s.scale = Math.min(1.6, Math.max(1, s.W / 900));
    s.ctx = canvas.getContext("2d");
    if (s.ctx) s.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }

  function draw(time: number) {
    const s = sim();
    const ctx = s.ctx;
    if (!ctx) return;
    const { W, H, nodes, byId } = s;

    // Gentle pointer parallax, eased so it breathes rather than tracks.
    s.parallaxX = lerp(s.parallaxX, s.targetParallaxX, 0.06);
    s.parallaxY = lerp(s.parallaxY, s.targetParallaxY, 0.06);

    ctx.fillStyle = TERMINAL_GROUND.background;
    ctx.fillRect(0, 0, W, H);

    // Ambient wash behind the center — the pre-rendered halo sprite, big.
    const centerSprite = spriteFor("#7fdcc9");
    if (centerSprite) {
      ctx.globalAlpha = 0.05 + Math.sin(time * 0.3) * 0.015;
      const hw = Math.max(W, H) * 0.55;
      ctx.drawImage(centerSprite, W / 2 - hw, H / 2 - hw, hw * 2, hw * 2);
      ctx.globalAlpha = 1;
    }

    // Star-dust ground texture: static positions, slow twinkle, faint
    // counter-parallax for depth. Texture only — never data.
    for (let i = 0; i < s.dust.length; i++) {
      const d = s.dust[i];
      const twinkle = 0.045 + (Math.sin(time * 0.4 + d.phase) + 1) * 0.022;
      ctx.fillStyle = styleAt(s.edgeTable, twinkle * d.depth);
      ctx.beginPath();
      ctx.arc(
        d.fx * W - s.parallaxX * 10 * d.depth,
        d.fy * H - s.parallaxY * 8 * d.depth,
        0.6 + d.depth * 0.7,
        0,
        Math.PI * 2,
      );
      ctx.fill();
    }

    // Per-node drawn position: base + depth-scaled lissajous drift + parallax.
    for (let i = 0; i < nodes.length; i++) {
      const n = nodes[i];
      const amp = DRIFT_AMP * n.z * (n.kind === "run" ? 1.35 : 1);
      n.dx =
        n.bx +
        Math.cos(time * 0.24 * n.driftSpeed + n.phase) * amp +
        s.parallaxX * 16 * (n.z - 1);
      n.dy =
        n.by +
        Math.sin(time * 0.19 * n.driftSpeed + n.phase) * amp +
        s.parallaxY * 12 * (n.z - 1);
      if (n.surge > 0) n.surge = Math.max(0, n.surge - 0.016);
    }

    // Entrance + resolution alpha, needed by edges and nodes alike.
    const nodeAlpha = (n: SimNode): number => {
      let alpha = 1;
      if (time < n.appearAt) return 0;
      const enter = (time - n.appearAt) / ENTRANCE_DURATION;
      if (enter < 1) alpha *= easeOutCubic(Math.max(0, enter));
      if (n.resolved) {
        const dt = time - n.resolved.at;
        const fadeFrom = n.resolved.outcome === "completed" ? 1.1 : 0.7;
        const fadeLen = n.resolved.outcome === "completed" ? 1.6 : 1.7;
        if (dt > fadeFrom) alpha *= Math.max(0, 1 - (dt - fadeFrom) / fadeLen);
      }
      return alpha;
    };

    // --- Edges (quantized-alpha strokes; no gradients, no string building) ---
    ctx.lineWidth = 1.25;
    for (let i = 0; i < nodes.length; i++) {
      const n = nodes[i];
      if (!n.anchorId) continue;
      const anchor = byId.get(n.anchorId);
      if (!anchor) continue;
      const a = nodeAlpha(n) * Math.min(1, nodeAlpha(anchor) + 0.2);
      if (a <= 0.01) continue;
      const pulse = (Math.sin(time * 0.4 + n.edgePhase) + 1) / 2;
      const alpha = (0.13 + pulse * 0.11 + Math.min(n.surge, 1) * 0.24) * a;
      ctx.strokeStyle = styleAt(s.edgeTable, alpha);
      ctx.beginPath();
      ctx.moveTo(n.dx, n.dy);
      ctx.lineTo(anchor.dx, anchor.dy);
      ctx.stroke();
    }

    // --- Particles: one per committed event, travelling its run's edge ---
    for (let i = 0; i < s.particles.length; i++) {
      const part = s.particles[i];
      if (!part.active) continue;
      const from = byId.get(part.fromId);
      const to = byId.get(part.toId);
      if (!from || !to) {
        part.active = false;
        continue;
      }
      const x = lerp(from.dx, to.dx, part.p);
      const y = lerp(from.dy, to.dy, part.p);
      const fade = Math.sin(part.p * Math.PI);
      const sprite = spriteFor(part.color);
      const pr = 9 * s.scale;
      if (sprite) {
        ctx.globalAlpha = 0.85 * fade;
        ctx.drawImage(sprite, x - pr, y - pr, pr * 2, pr * 2);
        ctx.globalAlpha = 1;
      }
      ctx.fillStyle = styleAt(tableFor(part.color), 0.95 * fade);
      ctx.beginPath();
      ctx.arc(x, y, 2.2 * s.scale, 0, Math.PI * 2);
      ctx.fill();
    }

    // --- Nodes ---
    const inspectedId = inspectedIdRef.current;
    for (let i = 0; i < nodes.length; i++) {
      const n = nodes[i];
      const a = nodeAlpha(n);
      if (a <= 0.01) continue;
      const enter = Math.min(1, Math.max(0, (time - n.appearAt) / ENTRANCE_DURATION));
      const scale = easeOutCubic(enter);
      const breathe = 1 + Math.sin(time * 0.7 + n.phase) * 0.05;
      const r = n.r * breathe * scale;
      const glow = (Math.sin(time * 0.8 + n.glowPhase) + 1) / 2;
      const table = tableFor(n.color);
      const sprite = spriteFor(n.color);

      let color = n.color;
      let flare = 0;
      if (n.resolved) {
        const dt = time - n.resolved.at;
        if (n.resolved.outcome === "completed") {
          color = RUN_SETTLE_COLOR;
          // The settle: one soft expanding ring — something WORKED.
          if (dt < 1.4) {
            const ring = dt / 1.4;
            ctx.strokeStyle = styleAt(tableFor(RUN_SETTLE_COLOR), (1 - ring) * 0.6 * a);
            ctx.lineWidth = 1.5;
            ctx.beginPath();
            ctx.arc(n.dx, n.dy, r + ring * 26, 0, Math.PI * 2);
            ctx.stroke();
          }
        } else if (n.resolved.outcome === "failed") {
          color = RUN_FLARE_COLOR;
          // A brief warm flare, dignified rather than alarming.
          flare = dt < 0.75 ? Math.sin((dt / 0.75) * Math.PI) : 0;
        }
      }
      const drawTable = color === n.color ? table : tableFor(color);
      const drawSprite = color === n.color ? sprite : spriteFor(color);

      if (drawSprite) {
        const halo = r * (n.kind === "center" ? 3.2 : 2.6) * (1 + flare * 0.8);
        ctx.globalAlpha =
          (0.16 + glow * 0.08 + flare * 0.45 + Math.min(n.surge, 1) * 0.12) * n.dim * a;
        ctx.drawImage(drawSprite, n.dx - halo, n.dy - halo, halo * 2, halo * 2);
        ctx.globalAlpha = 1;
      }

      ctx.globalAlpha = a;
      ctx.fillStyle = TERMINAL_GROUND.background;
      ctx.strokeStyle = styleAt(drawTable, (0.65 + glow * 0.35) * n.dim);
      ctx.lineWidth = n.id === inspectedId ? 2.4 : 1.5;
      if (n.square) {
        const side = r * 1.8;
        ctx.beginPath();
        if (typeof ctx.roundRect === "function") {
          ctx.roundRect(n.dx - side / 2, n.dy - side / 2, side, side, 4);
        } else {
          ctx.rect(n.dx - side / 2, n.dy - side / 2, side, side);
        }
        ctx.fill();
        ctx.stroke();
      } else {
        ctx.beginPath();
        ctx.arc(n.dx, n.dy, r, 0, Math.PI * 2);
        ctx.fill();
        ctx.stroke();
      }

      // The glowing core, pulsing via alpha only. The center skips it: the
      // AgentCulture mark below is its glyph, and a core dot would smudge it.
      if (n.kind !== "center") {
        ctx.fillStyle = styleAt(drawTable, (0.6 + glow * 0.4) * n.dim);
        ctx.beginPath();
        ctx.arc(n.dx, n.dy, Math.max(1.6, r * 0.32), 0, Math.PI * 2);
        ctx.fill();
      }
      ctx.globalAlpha = 1;

      // The control plane carries the AgentCulture mark as its glyph.
      if (n.kind === "center" && s.markPath) {
        const ms = (r * 2.4) / 64;
        ctx.save();
        ctx.globalAlpha = a;
        ctx.translate(n.dx - 32 * ms, n.dy - 32 * ms);
        ctx.scale(ms, ms);
        ctx.strokeStyle = styleAt(s.edgeTable, 0.5);
        ctx.lineWidth = 2.5 / ms;
        ctx.lineCap = "round";
        ctx.stroke(s.markPath);
        ctx.fillStyle = styleAt(tableFor("#7fdcc9"), 0.9);
        for (let m = 0; m < MARK_NODES.length; m++) {
          const [mx, my, mr] = MARK_NODES[m];
          ctx.beginPath();
          ctx.arc(mx, my, mr, 0, Math.PI * 2);
          ctx.fill();
        }
        ctx.restore();
      }
    }

    // Keep the DOM tooltip pinned to the inspected node as it drifts.
    if (inspectedId) {
      const n = byId.get(inspectedId);
      const tip = tooltipRef.current;
      if (n && tip) {
        tip.style.transform = `translate(${Math.round(n.dx)}px, ${Math.round(n.dy + n.r + 12)}px)`;
      }
    }
  }

  function step(dt: number) {
    const s = sim();
    for (let i = 0; i < s.particles.length; i++) {
      const part = s.particles[i];
      if (!part.active) continue;
      part.p += part.speed * dt;
      if (part.p >= 1) part.active = false;
    }
  }

  function frame(now: number) {
    const s = sim();
    if (!s.running) return;
    s.raf = requestAnimationFrame(frame);
    const time = (now - s.t0) / 1000;
    const dt = Math.min(0.05, time - (s.lastT ?? time));
    s.lastT = time;
    step(dt);
    draw(time);
  }

  function start() {
    const s = sim();
    if (s.running || reducedMotion || !s.visible) return;
    if (typeof cancelAnimationFrame !== "undefined") cancelAnimationFrame(s.raf);
    s.running = true;
    const base = typeof performance === "undefined" ? 0 : performance.now();
    // Resume where the clock left off so phases/entrances don't jump.
    s.t0 = base - (s.lastT ?? 0) * 1000;
    s.raf = requestAnimationFrame(frame);
  }

  function stop() {
    const s = sim();
    s.running = false;
    if (typeof cancelAnimationFrame !== "undefined") cancelAnimationFrame(s.raf);
  }

  /** The reduced-motion contract: one settled frame, no loop. */
  function renderStatic() {
    const s = sim();
    // Entrances resolved, drift at time 0: dignified and complete.
    draw(Math.max(1, s.nextAppear + ENTRANCE_DURATION + 0.05));
  }

  // Mount: size, observers, loop lifecycle.
  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap) return;
    const s = sim();
    resize();
    reconcile();
    if (reducedMotion) {
      stop();
      renderStatic();
    } else {
      start();
    }

    const ro =
      typeof ResizeObserver === "undefined"
        ? null
        : new ResizeObserver(() => {
            resize();
            reconcile(); // deterministic layout: recompute, never re-randomize
            if (reducedMotion || !s.running) renderStatic();
          });
    ro?.observe(wrap);

    const io =
      typeof IntersectionObserver === "undefined"
        ? null
        : new IntersectionObserver(
            (entries) => {
              s.visible = entries[0]?.isIntersecting ?? true;
              if (!s.visible) stop();
              else start();
            },
            { threshold: 0 },
          );
    io?.observe(wrap);

    const onVis = () => {
      if (document.hidden) stop();
      else start();
    };
    document.addEventListener("visibilitychange", onVis);

    return () => {
      stop();
      ro?.disconnect();
      io?.disconnect();
      document.removeEventListener("visibilitychange", onVis);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reducedMotion]);

  // Graph changes: reconcile (new nodes get their own entrance).
  useEffect(() => {
    graphRef.current = graph;
    reconcile();
    if (reducedMotion || !sim().running) renderStatic();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graph, reducedMotion]);

  // Install this canvas as the route's action listener.
  useEffect(() => {
    bus.listener = handleAction;
    return () => {
      if (bus.listener === handleAction) bus.listener = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bus, reducedMotion]);

  // --- Hover / keyboard inspection (labels on hover AND focus) ---

  function nodeAt(x: number, y: number): SimNode | null {
    const s = sim();
    let best: SimNode | null = null;
    let bestD = Infinity;
    for (let i = 0; i < s.nodes.length; i++) {
      const n = s.nodes[i];
      const dx = n.dx - x;
      const dy = n.dy - y;
      const d = Math.sqrt(dx * dx + dy * dy);
      if (d < Math.max(14, n.r + 8) && d < bestD) {
        best = n;
        bestD = d;
      }
    }
    return best;
  }

  function inspect(node: SimNode | null) {
    const nextId = node?.id ?? null;
    if (inspectedIdRef.current === nextId) return;
    inspectedIdRef.current = nextId;
    setInspected(node);
    const tip = tooltipRef.current;
    if (node && tip) {
      tip.style.transform = `translate(${Math.round(node.dx)}px, ${Math.round(node.dy + node.r + 12)}px)`;
    }
    if (reducedMotion) renderStatic();
  }

  function onPointerMove(event: React.PointerEvent<HTMLDivElement>) {
    const s = sim();
    const rect = event.currentTarget.getBoundingClientRect();
    const x = event.clientX - rect.left;
    const y = event.clientY - rect.top;
    s.targetParallaxX = (x / Math.max(1, s.W)) * 2 - 1;
    s.targetParallaxY = (y / Math.max(1, s.H)) * 2 - 1;
    inspect(nodeAt(x, y));
  }

  function onPointerLeave() {
    const s = sim();
    s.targetParallaxX = 0;
    s.targetParallaxY = 0;
    inspect(null);
  }

  function onKeyDown(event: React.KeyboardEvent<HTMLDivElement>) {
    const s = sim();
    if (event.key === "Escape") {
      inspect(null);
      return;
    }
    if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return;
    event.preventDefault();
    if (s.nodes.length === 0) return;
    const order = s.nodes;
    const idx = order.findIndex((n) => n.id === inspectedIdRef.current);
    const delta = event.key === "ArrowRight" ? 1 : -1;
    const next = order[(idx + delta + order.length) % order.length];
    inspect(next);
  }

  return (
    <div
      ref={wrapRef}
      id="mesh-canvas"
      className="mesh-canvas canvas-surface"
      data-motion={reducedMotion ? "static" : "animated"}
      tabIndex={0}
      role="img"
      aria-label={
        "Living map of the Culture Nodes mesh: the control plane at the " +
        "center, registered actors orbiting it, active runs as satellites " +
        "of the actors executing them. Particles travel the links as " +
        "committed events arrive. Arrow keys inspect each node."
      }
      onPointerMove={onPointerMove}
      onPointerLeave={onPointerLeave}
      onBlur={() => inspect(null)}
      onKeyDown={onKeyDown}
    >
      <canvas ref={canvasRef} aria-hidden="true" />
      <div
        ref={tooltipRef}
        id="mesh-tooltip"
        className="mesh-tooltip"
        role="status"
        data-node-kind={inspected?.kind ?? ""}
        hidden={!inspected}
      >
        {inspected ? (
          <>
            <span className="mesh-tooltip__label">{inspected.label}</span>
            <span className="mesh-tooltip__sub">{inspected.sub}</span>
            {inspected.category ? (
              <CategoryChip category={inspected.category} />
            ) : null}
          </>
        ) : null}
      </div>
    </div>
  );
}

export default MeshCanvas;
