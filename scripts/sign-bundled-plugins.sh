#!/bin/sh
# Sign every Bundled plugin with the Obelo release key (ADR-0069 Q8,
# .scratch/plugin-inplace-upgrade issue 02).
#
#   usage: scripts/sign-bundled-plugins.sh <modules-dir>
#
# Run from the repository root AFTER scripts/build-bundled-plugins.sh, on the
# directory internal/bundled embeds. For each <id>.wasm.gz it writes <id>.sig.json
# beside it: a detached signature over the manifest and the DECOMPRESSED module (the
# bytes boot writes to disk), made by cmd/pluginsign, publisher "Obelo".
#
# THE KEY IS OPTIONAL AND THIS IS NOT AN ERROR WITHOUT IT. A dev build has no key; the
# script then removes any stale signature files and exits 0, and Bundled plugins ship
# unsigned. The private key comes from, in order:
#   OBELO_RELEASE_SIGNING_KEY_FILE  a file holding it (the Dockerfile's BuildKit
#                                   secret mount, /run/secrets/release_sign_key)
#   OBELO_RELEASE_SIGNING_KEY       the value itself (local release builds and CI)
# It is copied nowhere but a 0600 file in a private temp directory that is removed on
# exit, is never echoed, and is never passed as an argument (so not in `ps`).
set -eu

out_dir=${1:-internal/bundled/modules}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

key_file=${OBELO_RELEASE_SIGNING_KEY_FILE:-}
if [ -z "$key_file" ] || [ ! -s "$key_file" ]; then
  key_file=
  if [ -n "${OBELO_RELEASE_SIGNING_KEY:-}" ]; then
    key_file=$tmp/release.key
    ( umask 077; printf '%s\n' "$OBELO_RELEASE_SIGNING_KEY" > "$key_file" )
  fi
fi

if [ -z "$key_file" ]; then
  rm -f "$out_dir"/*.sig.json
  echo "no release signing key: Bundled plugins in $out_dir are left unsigned"
  exit 0
fi

go build -o "$tmp/pluginsign" ./cmd/pluginsign

count=0
for gz in "$out_dir"/*.wasm.gz; do
  [ -f "$gz" ] || continue
  id=$(basename "$gz" .wasm.gz)
  gzip -dc "$gz" > "$tmp/$id.wasm"
  "$tmp/pluginsign" sign -key "$key_file" -publisher Obelo \
    -manifest "$out_dir/$id.manifest.json" -module "$tmp/$id.wasm" \
    -out "$out_dir/$id.sig.json"
  count=$((count + 1))
done

if [ "$count" -eq 0 ]; then
  echo "ERROR: a release signing key was supplied but $out_dir holds no module to sign" >&2
  exit 1
fi
echo "ok: signed $count bundled plugin(s) in $out_dir as Obelo"
