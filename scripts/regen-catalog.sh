#!/usr/bin/env bash
# Regenerates shop/catalog.go (and the golden packets the parity test pins) from
# the TypeScript reference server OpenCombas/open-combas-server-js.
#
# Requirements: node, and esbuild resolvable from the js checkout
# (npm i --no-save esbuild, or a global install).
#
# Usage: scripts/regen-catalog.sh [path-to-open-combas-server-js]
set -euo pipefail

JS_REPO="$(cd "${1:-../open-combas-server-js}" 2>/dev/null && pwd)" || true
: "${JS_REPO:?usage: scripts/regen-catalog.sh <path-to-open-combas-server-js> (default ../open-combas-server-js)}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [ ! -f "$JS_REPO/src/services/shop-response.ts" ]; then
  echo "error: $JS_REPO does not look like open-combas-server-js" >&2
  exit 1
fi

echo "==> extracting catalog data from $JS_REPO"
node -e '
const fs = require("fs");
const partsFile = fs.readFileSync(process.argv[1] + "/src/data/parts.ts", "utf8");
const houndsFile = fs.readFileSync(process.argv[1] + "/src/data/shop-hounds.ts", "utf8");
const parts = [];
let cur = null;
for (const raw of partsFile.split("\n")) {
  const line = raw.trim();
  let m = line.match(/^part_id:\s*"([^"]*)"/);
  if (m) { if (cur) parts.push(cur); cur = {part_id: m[1]}; continue; }
  m = line.match(/^faction:\s*"([^"]*)"/); if (m && cur) { cur.faction = m[1]; continue; }
  m = line.match(/^price:\s*(\d+|null)/); if (m && cur) { cur.price = m[1] === "null" ? 0 : +m[1]; continue; }
}
if (cur) parts.push(cur);
const hounds = {};
const re = /(A|B|C):\s*\[([^\]]*)\]/g; let m;
while ((m = re.exec(houndsFile))) hounds[m[1]] = [...m[2].matchAll(/'"'"'([A-Z]{2}_[A-Z]{2}\d+)'"'"'/g)].map(x => x[1]);
fs.writeFileSync("/tmp/ocb-parts.json", JSON.stringify(parts));
fs.writeFileSync("/tmp/ocb-hounds.json", JSON.stringify(hounds));
console.error("extracted", parts.length, "parts");
' "$JS_REPO"

echo "==> generating shop/catalog.go"
node -e '
const fs = require("fs");
const parts = JSON.parse(fs.readFileSync("/tmp/ocb-parts.json", "utf8"));
const hounds = JSON.parse(fs.readFileSync("/tmp/ocb-hounds.json", "utf8"));
const rows = parts.filter(p => p.part_id).map(p => `\t{Stem: ${JSON.stringify(p.part_id)}, Faction: ${JSON.stringify(p.faction)}, Price: ${p.price || 0}},`);
let out = `package shop

// Code generated from OpenCombas/open-combas-server-js (src/data/parts.ts,
// src/data/shop-hounds.ts). DO NOT EDIT. Regenerate with
// scripts/regen-catalog.sh (run from this directory).

// Part is one entry of the in-game shop catalog.
type Part struct {
\tStem    string // asset stem, e.g. "CK_CA001"
\tFaction string // "A" Tarakia, "B" Morskoj, "C" Sal Kar
\tPrice   uint32 // buy price; 0 = the source data lists no price
}

// Parts is the full catalog, in source order.
var Parts = []Part{
${rows.join("\n")}
}

// houndStems lists the parts a faction'"'"'s complete-hound presets need on the
// buy lineup. The client skips a preset unless every one of these is for sale.
// Joints are omitted: the client does not require them.
var houndStems = map[string][]string{
`;
for (const f of ["A", "B", "C"]) {
  const stems = hounds[f];
  out += `\t${JSON.stringify(f)}: {`;
  for (let i = 0; i < stems.length; i += 4) {
    out += "\n\t\t" + stems.slice(i, i + 4).map(s => JSON.stringify(s)).join(", ") + ",";
  }
  out += "\n\t},\n";
}
out += `}

// hitPartStem is the part advertised in the "one hit" slot of the buy list.
// When it is not on sale for a faction the first row of that lineup is used.
const hitPartStem = "WH_HC001"
`;
fs.writeFileSync(process.argv[1] + "/shop/catalog.go", out);
console.error("wrote shop/catalog.go");
' "$REPO_ROOT"

echo "==> regenerating golden packets"
# The generator imports esbuild as an ESM package; ESM ignores NODE_PATH, so
# when the checkout has no local esbuild we link the caller's global install
# into a temporary node_modules and clean it up afterwards.
if [ ! -d "$JS_REPO/node_modules/esbuild" ]; then
  ESBUILD_SRC="$(node -e 'process.stdout.write(require.resolve("esbuild/package.json").replace(/\/package\.json$/,""))' 2>/dev/null || true)"
  if [ -z "$ESBUILD_SRC" ]; then
    echo "error: esbuild not found; run: npm i --no-save esbuild (in $JS_REPO) or npm i -g esbuild" >&2
    exit 1
  fi
  LINKED=1
  mkdir -p "$JS_REPO/node_modules"
  ln -sfn "$ESBUILD_SRC" "$JS_REPO/node_modules/esbuild"
  trap '[ -n "${LINKED:-}" ] && rm -f "$JS_REPO/node_modules/esbuild"' EXIT
fi
(cd "$JS_REPO" && node scripts/gen-golden.mjs "$REPO_ROOT/shop/testdata")

echo "==> formatting and verifying"
gofmt -w "$REPO_ROOT/shop/catalog.go"
(cd "$REPO_ROOT" && go test ./shop/ -run TestBuildResponseMatchesGolden -v)

echo "==> done. Review the diff, then commit."
