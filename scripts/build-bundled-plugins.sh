#!/bin/sh
# Build every Bundled plugin into the directory internal/bundled embeds
# (ADR-0059 decision 10, .scratch/bundled-plugins).
#
# THIS IS THE ONE DEFINITION OF THE BUILD COMMAND. `make plugins` runs it and so
# does the Dockerfile's `plugins` stage, because for one release the two were
# separate copies of the same five-line loop and "keep them in step" was a comment
# rather than a mechanism. The image is the artifact that matters here: without a
# plugin build it compiles, boots, scans and silently enriches NOTHING, because
# internal/bundled/modules/ is gitignored and arrives empty in the COPY — the exact
# shape CLAUDE.md records twice.
#
#   usage: scripts/build-bundled-plugins.sh <out-dir> <work-dir>
#
#     out-dir   where <id>.wasm.gz and <id>.manifest.json are written; this is
#               internal/bundled/modules, the embedded directory.
#     work-dir  where the uncompressed .wasm lands on its way to being gzipped.
#               NOT inside out-dir: that whole directory is embedded, so a stray
#               .wasm left beside the .wasm.gz would be compiled into the binary
#               twice over.
#
# Run it from the repository root. DISCOVERY IS THE FILESYSTEM: a plugin is a
# directory under plugins/ with a manifest.json in it, and nothing here, in the
# Makefile or in the Dockerfile names one.
#
# THE BUILD COMMAND IS THE REFERENCE PLUGIN'S OWN, character for character:
#
#     GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= go build -buildmode=c-shared
#
# That is not a coincidence to be tidied away. It is the claim ADR-0058 makes —
# that a plugin an outsider writes and a plugin the maintainer ships are built the
# same way, with stock Go and no PDK. The moment this needs a flag the authoring
# guide does not mention, the claim is false.
#
# GOFLAGS= is emptied on purpose. It is inherited from the environment (CI sets
# -buildvcs=false, the amd64 gate sets it too), and a flag meant for the server's
# build reaching a wasip1 c-shared build is a failure nobody would connect to the
# variable that caused it. GOWORK is left alone: plugins/<id>/go.mod carries
# `replace` lines for both local modules, so it resolves with or without a
# workspace around it.
#
# The output is <id>.wasm.gz (gzip -9) plus <id>.manifest.json, byte for byte as
# the author wrote it. Compressed because a stock-Go guest is ~3.9 MB and about a
# megabyte gzipped, and seven of them uncompressed would be most of the binary's
# size for bytes that are written to disk at most once in a server's life;
# `gzip -n` so the archive carries no timestamp and two builds of the same module
# are the same file.
#
# THE DIRECTORY NAME IS THE ID. The manifest's own id must match it, checked here
# rather than at boot, because the loader refuses a mismatch and "your plugin
# silently did not appear" is a worse place to learn it than a build failure.
set -eu

out_dir=${1:-internal/bundled/modules}
work_dir=${2:-bin/plugins}

# The go build below runs with `cd plugins/<id>`, so -o must be absolute.
case $work_dir in
  /*) abs_work=$work_dir ;;
  *)  abs_work=$(pwd)/$work_dir ;;
esac

mkdir -p "$out_dir" "$work_dir"

count=0
for manifest in plugins/*/manifest.json; do
  [ -f "$manifest" ] || continue
  dir=$(dirname "$manifest")
  id=$(basename "$dir")
  declared=$(sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" | head -1)
  if [ "$declared" != "$id" ]; then
    echo "ERROR: $manifest declares the id \"$declared\" but the directory is \"$id\"." >&2
    echo "       They must match: the directory IS the id on disk and the key its settings row is written under." >&2
    exit 1
  fi
  echo "building $dir -> $out_dir/$id.wasm.gz"
  ( cd "$dir" && GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= \
      go build -buildmode=c-shared -o "$abs_work/$id.wasm" . )
  gzip -9nc "$abs_work/$id.wasm" > "$out_dir/$id.wasm.gz"
  cp "$manifest" "$out_dir/$id.manifest.json"
  count=$((count + 1))
done

if [ "$count" -eq 0 ]; then
  echo "ERROR: no plugins/*/manifest.json found — is this the repository root?" >&2
  exit 1
fi

echo "ok: $count bundled plugin(s) built into $out_dir"
