import { chromium } from "playwright";
const b = await chromium.launch(); const p = await b.newPage();
const pending = new Set(); p.on("request", r => pending.add(r.url())); p.on("requestfinished", r => pending.delete(r.url())); p.on("requestfailed", r => pending.delete(r.url()));
const t0 = Date.now(); await p.goto("http://127.0.0.1:4173/", { waitUntil: "load" });
const first = JSON.parse(await p.locator("#agent-state").textContent()).status;
let status = first; for (let i = 0; i < 60 && status !== "ready"; i++) { await p.waitForTimeout(100); status = JSON.parse(await p.locator("#agent-state").textContent()).status; }
console.log("branch: at load =", first, "; status", status, "after ~", Date.now()-t0, "ms; still-pending requests:", [...pending].map(u=>u.replace("http://127.0.0.1:4173","")).join(", "));
await p.waitForTimeout(3000); console.log("pending after +3s:", [...pending].map(u=>u.replace("http://127.0.0.1:4173","")).join(", "));
await b.close();
