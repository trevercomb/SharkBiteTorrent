#!/usr/bin/env bash
# Checks that third_party/anacrolix-torrent is exactly the published
# anacrolix/torrent version go.mod requires, plus the documented patch
# (PATCHED.md: upstream's fix 76452a2c8a2f) and nothing else. Run from the
# repo root; CI runs it on every push.
set -euo pipefail

ver=$(sed -n 's#^[[:space:]]*github.com/anacrolix/torrent \(v[^ ]*\).*#\1#p' go.mod | head -1)
[ -n "$ver" ] || { echo "go.mod doesn't require github.com/anacrolix/torrent" >&2; exit 1; }
grep -q "anacrolix/torrent $ver" third_party/anacrolix-torrent/PATCHED.md || {
  echo "go.mod requires anacrolix/torrent $ver, but third_party/anacrolix-torrent is a patched copy of another version (see its PATCHED.md)" >&2
  exit 1
}

upstream=$(go mod download -json "github.com/anacrolix/torrent@$ver" | sed -n 's/.*"Dir": "\(.*\)".*/\1/p' | sed 's#\\\\#/#g')
[ -d "$upstream" ] || { echo "couldn't download anacrolix/torrent $ver" >&2; exit 1; }

# The patched files, exactly as reviewed (sha256, line endings ignored).
want="
bencode/decode.go 78e0b96a00648754fb2e8c2a0ac219d9ac3faf0befd05628e289d5597bf6a0bc
bencode/decode_test.go 9deed8295f518af7c0d224ec69a80ae0f6680f208c55502dac322f7134945ddf
bencode/fuzz_test.go ad097d0fb678d07c9e4bf629b77a020e174fe50cc03aa9f3bb1e0b5c93799fa7
metainfo/recursion_test.go 827dae901a6b7c3fd98b05ec963e6265b221521c4990129fa911e096cdcd7e00
"

problems=0
# Every other file must match upstream (ignoring line endings).
while IFS= read -r line; do
  case "$line" in
    "Only in third_party/anacrolix-torrent: PATCHED.md") ;;
    "Only in third_party/anacrolix-torrent/metainfo: recursion_test.go") ;;
    "Files "*" and third_party/anacrolix-torrent/bencode/decode.go differ") ;;
    "Files "*" and third_party/anacrolix-torrent/bencode/decode_test.go differ") ;;
    "Files "*" and third_party/anacrolix-torrent/bencode/fuzz_test.go differ") ;;
    *) echo "unexpected difference from upstream: $line" >&2; problems=1 ;;
  esac
done < <(diff -rq --strip-trailing-cr "$upstream" third_party/anacrolix-torrent || true)

while read -r f hash; do
  [ -n "$f" ] || continue
  got=$(tr -d '\r' < "third_party/anacrolix-torrent/$f" | sha256sum | cut -c1-64)
  if [ "$got" != "$hash" ]; then
    echo "$f isn't the reviewed version (sha256 $got); if you changed it on purpose, review it and update its hash here" >&2
    problems=1
  fi
done <<< "$want"

# govulncheck can't match advisories to a directory replacement, so ask OSV
# (which Go's vulnerability database feeds) about $ver directly: every
# advisory must be one PATCHED.md says this copy fixes (by any of its IDs).
# No answer fails too: a check that silently passes is worse than none.
osv=$(curl -fsS --max-time 30 -X POST https://api.osv.dev/v1/query \
  -d "{\"package\":{\"name\":\"github.com/anacrolix/torrent\",\"ecosystem\":\"Go\"},\"version\":\"${ver#v}\"}") || {
  echo "couldn't ask OSV about anacrolix/torrent $ver's advisories" >&2
  exit 1
}
unfixed=$(printf '%s' "$osv" | python3 -c '
import json, sys
patched = open("third_party/anacrolix-torrent/PATCHED.md", encoding="utf-8").read()
for v in json.load(sys.stdin).get("vulns", []):
    ids = [v["id"], *v.get("aliases", [])]
    if not any(i in patched for i in ids):
        print(" / ".join(ids))
')
if [ -n "$unfixed" ]; then
  echo "anacrolix/torrent $ver has advisories this copy doesn't fix (see PATCHED.md):" >&2
  echo "$unfixed" >&2
  problems=1
fi

[ "$problems" = 0 ] && echo "third_party/anacrolix-torrent = upstream $ver + the documented patch; no unfixed advisories"
exit "$problems"
