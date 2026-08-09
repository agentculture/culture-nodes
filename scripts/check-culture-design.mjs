#!/usr/bin/env node
// scripts/check-culture-design.mjs
//
// Verifies web/src/culture-design/ stays faithful to the pinned
// agentculture/org revision recorded in docs/adr/0001-culture-design-source.md
// (task t5). Three checks, each fatal on failure:
//
//   (a) tokens.css's copied body is byte-identical to org's
//       site-astro/src/styles/global.css AT THE RECORDED PIN (read via
//       `git show <pin>:<path>`, never org's working tree / current HEAD —
//       org's HEAD is free to move on without failing this check).
//   (b) every hex color literal in palette.ts appears somewhere in the org
//       source files it was extracted from, at the same pin.
//   (c) every stroke-dasharray value in edges.ts matches the org diagram's
//       (CliRuntimeStackDiagram.astro) dasharray conventions, at the same
//       pin.
//
// Run with: node scripts/check-culture-design.mjs
// No dependencies beyond Node's own stdlib + a `git` binary on PATH.

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import path from "node:path";
import assert from "node:assert/strict";

const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(SCRIPT_DIR, "..");
const CULTURE_DESIGN_DIR = path.join(REPO_ROOT, "web", "src", "culture-design");
const ADR_PATH = path.join(REPO_ROOT, "docs", "adr", "0001-culture-design-source.md");

// The org checkout is a fixed sibling repo on this machine, per the ADR
// and the t5 task description. Overridable for anyone running this from a
// different checkout layout.
const ORG_REPO = process.env.CULTURE_DESIGN_ORG_REPO ?? "/home/spark/git/org";
const ORG_TOKENS_PATH = "site-astro/src/styles/global.css";
const ORG_COLLEAGUE_TERMINAL_PATH = "site-astro/src/components/ColleagueTerminal.astro";
const ORG_CLI_RUNTIME_DIAGRAM_PATH = "site-astro/src/components/CliRuntimeStackDiagram.astro";

let failures = 0;
let checks = 0;

function ok(label) {
  checks += 1;
  console.log(`ok   - ${label}`);
}

function fail(label, detail) {
  checks += 1;
  failures += 1;
  console.error(`FAIL - ${label}`);
  if (detail) console.error(`       ${detail}`);
}

function run(check, label) {
  try {
    check();
    ok(label);
  } catch (err) {
    fail(label, err && err.message ? err.message : String(err));
  }
}

function sha256(text) {
  return createHash("sha256").update(text, "utf8").digest("hex");
}

/** Read a file's content at a specific commit via `git show`, never the
 *  working tree — so the check is stable even after org's HEAD moves on. */
function readOrgFileAtPin(relPath, pin) {
  return execFileSync("git", ["-C", ORG_REPO, "show", `${pin}:${relPath}`], {
    encoding: "utf8",
  });
}

/** Extract the pinned commit hash from the ADR — the ADR is the single
 *  source of truth for the pin, so the script never hardcodes it
 *  separately (that would let the two silently drift apart). */
function readPinFromAdr() {
  const adr = readFileSync(ADR_PATH, "utf8");
  const match = adr.match(/^pin:\s+([0-9a-f]{40})$/m);
  assert.ok(
    match,
    `could not find a 'pin:  <40-hex-char-sha>' line in ${ADR_PATH}`,
  );
  return match[1];
}

/** Extract the pinned commit hash tokens.css itself claims in its header
 *  comment, so we can assert it agrees with the ADR. */
function readPinFromTokensHeader(tokensCss) {
  const match = tokensCss.match(/^\s*\*\s*Pinned commit:\s*([0-9a-f]{40})\s*$/m);
  assert.ok(
    match,
    "tokens.css header comment is missing a 'Pinned commit:  <sha>' line",
  );
  return match[1];
}

/** Split tokens.css into [header, verbatimBody]. The verbatim body is
 *  everything after the header comment's own closing marker line — see
 *  the sentinel comment inside the header itself. */
function splitTokensCss(tokensCss) {
  const sentinel =
    "---- verbatim copy of site-astro/src/styles/global.css follows ----";
  const sentinelIdx = tokensCss.indexOf(sentinel);
  assert.ok(
    sentinelIdx >= 0,
    `tokens.css is missing the verbatim-copy sentinel comment ("${sentinel}")`,
  );
  const closeIdx = tokensCss.indexOf("*/", sentinelIdx);
  assert.ok(closeIdx >= 0, "tokens.css header comment is never closed with */");
  // Body starts right after the closing "*/" and its newline.
  const afterClose = tokensCss.indexOf("\n", closeIdx);
  assert.ok(afterClose >= 0, "tokens.css has no content after its header comment");
  const body = tokensCss.slice(afterClose + 1);
  const header = tokensCss.slice(0, afterClose + 1);
  return { header, body };
}

function extractHexColors(source) {
  const matches = source.match(/#[0-9a-fA-F]{6}\b/g) ?? [];
  return [...new Set(matches.map((h) => h.toLowerCase()))];
}

function extractDasharrays(source) {
  const matches = [
    ...source.matchAll(/strokeDasharray:\s*"([^"]+)"/g),
  ].map((m) => m[1]);
  return [...new Set(matches)];
}

// ---------------------------------------------------------------------

const pin = readPinFromAdr();
console.log(`culture-design check — pinned org commit ${pin}`);
console.log(`org repo: ${ORG_REPO}`);
console.log("");

const tokensCssPath = path.join(CULTURE_DESIGN_DIR, "tokens.css");
const paletteTsPath = path.join(CULTURE_DESIGN_DIR, "palette.ts");
const edgesTsPath = path.join(CULTURE_DESIGN_DIR, "edges.ts");

const tokensCss = readFileSync(tokensCssPath, "utf8");
const paletteTs = readFileSync(paletteTsPath, "utf8");
const edgesTs = readFileSync(edgesTsPath, "utf8");

// (0) tokens.css's own header agrees with the ADR about which commit is pinned.
run(() => {
  const headerPin = readPinFromTokensHeader(tokensCss);
  assert.equal(
    headerPin,
    pin,
    `tokens.css header says ${headerPin}, ADR says ${pin}`,
  );
}, "tokens.css header pin matches ADR pin");

// (a) tokens.css verbatim body is byte-identical to org's global.css AT THE PIN.
run(() => {
  const { body } = splitTokensCss(tokensCss);
  const orgSource = readOrgFileAtPin(ORG_TOKENS_PATH, pin);
  const bodyHash = sha256(body);
  const orgHash = sha256(orgSource);
  assert.equal(
    bodyHash,
    orgHash,
    `tokens.css body sha256=${bodyHash} != org ${ORG_TOKENS_PATH}@${pin.slice(0, 12)} sha256=${orgHash}`,
  );
  assert.equal(
    body,
    orgSource,
    "tokens.css body differs from org source (hash check above already caught this; string compare confirms)",
  );
}, `tokens.css is byte-identical to org ${ORG_TOKENS_PATH}@${pin.slice(0, 12)}`);

// (b) every hex in palette.ts appears in the org sources it was extracted from.
run(() => {
  const orgGlobalCss = readOrgFileAtPin(ORG_TOKENS_PATH, pin);
  const orgColleagueTerminal = readOrgFileAtPin(ORG_COLLEAGUE_TERMINAL_PATH, pin);
  const orgHaystack = (orgGlobalCss + "\n" + orgColleagueTerminal).toLowerCase();

  const paletteHexes = extractHexColors(paletteTs);
  assert.ok(paletteHexes.length > 0, "palette.ts has no hex color literals to check");

  const missing = paletteHexes.filter((hex) => !orgHaystack.includes(hex));
  assert.deepEqual(
    missing,
    [],
    `hex(es) in palette.ts not found in org sources (${ORG_TOKENS_PATH}, ${ORG_COLLEAGUE_TERMINAL_PATH}) @${pin.slice(0, 12)}: ${missing.join(", ")}`,
  );
  console.log(`       checked ${paletteHexes.length} distinct hex literal(s): ${paletteHexes.join(", ")}`);
}, "every hex in palette.ts traces back to an org source file");

// (c) edges.ts dasharray values match the org diagram conventions.
run(() => {
  const orgDiagram = readOrgFileAtPin(ORG_CLI_RUNTIME_DIAGRAM_PATH, pin);

  const edgeDasharrays = extractDasharrays(edgesTs);
  assert.ok(edgeDasharrays.length > 0, "edges.ts has no strokeDasharray values to check");

  // CliRuntimeStackDiagram.astro's own dotted-connector convention.
  assert.ok(
    orgDiagram.includes("stroke-dasharray: 2 7"),
    `org diagram (${ORG_CLI_RUNTIME_DIAGRAM_PATH}) no longer declares 'stroke-dasharray: 2 7'`,
  );
  // The "replaceable" dashed-border convention, repurposed by edges.ts's DASHED.
  assert.ok(
    orgDiagram.includes("stroke-dasharray: 9 7"),
    `org diagram (${ORG_CLI_RUNTIME_DIAGRAM_PATH}) no longer declares 'stroke-dasharray: 9 7'`,
  );

  for (const dasharray of edgeDasharrays) {
    assert.ok(
      orgDiagram.includes(`stroke-dasharray: ${dasharray}`),
      `edges.ts strokeDasharray "${dasharray}" has no matching 'stroke-dasharray: ${dasharray}' in ${ORG_CLI_RUNTIME_DIAGRAM_PATH}`,
    );
  }
  console.log(`       checked ${edgeDasharrays.length} dasharray value(s): ${edgeDasharrays.join(", ")}`);

  // SOLID's stroke-width traces to .connector.solid's stroke-width: 2.4.
  assert.ok(
    orgDiagram.includes("stroke-width: 2.4"),
    `org diagram (${ORG_CLI_RUNTIME_DIAGRAM_PATH}) no longer declares 'stroke-width: 2.4' for .connector.solid`,
  );
  assert.ok(
    edgesTs.includes("strokeWidth: 2.4"),
    "edges.ts SOLID no longer declares strokeWidth: 2.4",
  );
}, "edges.ts dasharray/width values match org's CliRuntimeStackDiagram.astro conventions");

console.log("");
console.log(`${checks - failures}/${checks} checks passed`);

if (failures > 0) {
  console.error(`${failures} check(s) FAILED`);
  process.exit(1);
}

console.log("culture-design check: PASS");
