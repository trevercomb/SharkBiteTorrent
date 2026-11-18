#!/usr/bin/env bash
# npm audit --audit-level=high, except for the advisories listed below.
# Run from the frontend folder (Checks and Release do).
#
# An advisory goes on the list only when it can't affect the app and has no
# fixed version yet. Give the reason, and remove it once a fix ships
# (Dependabot opens that update).
#
# GHSA-fc5q-39xc-vjpf picomatch ReDoS (all versions <= 4.0.7, no fix as of
#   2026-11-17). Only build tools use picomatch (vite, svelte-check), on the
#   project's own glob patterns; nothing of it is in the built frontend, and
#   `npm audit --omit=dev` finds nothing.
set -euo pipefail

IGNORED="GHSA-fc5q-39xc-vjpf"

trap 'rm -f npm-audit.json' EXIT
npm audit --json > npm-audit.json || true
IGNORED="$IGNORED" node -e '
const r = JSON.parse(require("fs").readFileSync("npm-audit.json", "utf8"));
if (r.error) { console.error("npm audit failed:", JSON.stringify(r.error)); process.exit(1); }
const ignored = new Set(process.env.IGNORED.split(/\s+/).filter(Boolean));
const seen = new Map();
for (const v of Object.values(r.vulnerabilities || {}))
  for (const a of v.via)
    if (typeof a === "object" && (a.severity === "high" || a.severity === "critical"))
      seen.set(a.url, a);
let bad = 0;
for (const [url, a] of seen) {
  const id = url.split("/").pop();
  if (ignored.has(id)) { console.log("ignored (see scripts/npm-audit.sh):", id, a.name, a.title); continue; }
  console.log("FAIL:", a.severity, a.name, a.range, a.title, url);
  bad++;
}
for (const id of ignored)
  if (![...seen.keys()].some(u => u.endsWith("/" + id)))
    console.log("note:", id, "no longer reported; remove it from scripts/npm-audit.sh");
if (bad) process.exit(1);
console.log("npm audit: no high or critical advisories" + (seen.size ? " apart from the ignored ones" : ""));
'
