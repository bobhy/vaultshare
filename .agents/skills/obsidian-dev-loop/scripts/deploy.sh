#!/usr/bin/env bash
# deploy.sh — copy this repo's built plugin into an Obsidian test vault and reload it.
#
# Usage (from anywhere inside the plugin repo):
#   .agents/skills/obsidian-dev-loop/scripts/deploy.sh <vault-folder> [--build]
#
#   <vault-folder>  folder name under ~/obsidian-test (e.g. test1)
#   --build         run `npm run build` first
#
# Copies main.js, manifest.json and styles.css (if present) into
# <vault>/.obsidian/plugins/<manifest id>/, never touching data.json, then
# enables the plugin if needed, reloads it and prints captured JS errors.
# All CLI access goes through `obsidian-testvault`, which refuses non-test vaults.
set -euo pipefail

die() { echo "deploy.sh: $*" >&2; exit 2; }

[[ $# -ge 1 ]] || die "usage: deploy.sh <vault-folder> [--build]"
vault="$1"; shift
build=false
for arg in "$@"; do
  case "$arg" in
    --build) build=true ;;
    *) die "unknown option: $arg" ;;
  esac
done

command -v obsidian-testvault >/dev/null || die "obsidian-testvault not found on PATH (expected in ~/.local/bin)"

repo="$(git rev-parse --show-toplevel 2>/dev/null)" || die "run from inside the plugin repository"
cd "$repo"

if $build; then npm run build; fi

[[ -f main.js && -f manifest.json ]] || die "main.js or manifest.json missing; run with --build"
id="$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' manifest.json)"

root="${OBSIDIAN_TEST_ROOT:-$HOME/obsidian-test}"
[[ "$vault" =~ ^[A-Za-z0-9._-]+$ && "$vault" != "." && "$vault" != ".." ]] || die "invalid vault folder name: $vault"
[[ -d "$root/$vault/.obsidian" ]] || die "$root/$vault is not an Obsidian vault (no .obsidian folder)"

dest="$root/$vault/.obsidian/plugins/$id"
mkdir -p "$dest"
files=(main.js manifest.json)
[[ -f styles.css ]] && files+=(styles.css)
cp -- "${files[@]}" "$dest/"
echo "copied ${files[*]} -> $dest"

# The CLI exits 0 even on failure, so check its output instead.
if ! obsidian-testvault "$vault" plugins:enabled filter=community | grep -qx "$id"; then
  echo "enabling $id"
  obsidian-testvault "$vault" plugin:enable id="$id" filter=community
fi
obsidian-testvault "$vault" dev:errors clear >/dev/null
obsidian-testvault "$vault" plugin:reload id="$id"
loaded="$(obsidian-testvault "$vault" eval code="app.plugins.plugins['$id'] ? 'loaded' : 'missing'")"
echo "plugin $id: $loaded"
echo "--- dev:errors after reload"
obsidian-testvault "$vault" dev:errors
[[ "$loaded" == *loaded* ]] || exit 1
