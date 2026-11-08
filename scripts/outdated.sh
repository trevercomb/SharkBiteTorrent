#!/usr/bin/env bash
# Lists the libraries with newer versions, for the release checklist (see
# RELEASING.md). A report, not a gate: read what each update changes and
# decide. Known vulnerabilities are a gate already (govulncheck, npm audit).
set -u
cd "$(dirname "$0")/.."

echo "== Go modules used directly (the patched copy in third_party excluded)"
go list -m -u -f '{{if and (not .Indirect) .Update (not .Replace)}}  {{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all | grep -v '^$' || echo "  all current"

echo "== Go toolchain"
want=$(sed -n 's/^toolchain go//p' go.mod)
latest=$(curl -sSf 'https://go.dev/VERSION?m=text' 2>/dev/null | head -1 | sed 's/^go//')
if [ -z "$latest" ]; then
  echo "  go.mod: $want (couldn't fetch the latest)"
elif [ "$want" = "$latest" ]; then
  echo "  $want (current)"
else
  echo "  $want -> $latest"
fi

echo "== wails CLI (pinned in the workflows; update with the library)"
pinned=$(sed -n 's/^  WAILS_VERSION: //p' .github/workflows/release.yml)
echo "  workflows: $pinned, go.mod: $(go list -m -f '{{.Version}}' github.com/wailsapp/wails/v2)"

echo "== go-webview2 (our patched copy in third_party; re-apply the patch to update)"
echo "  ours: $(go list -m -f '{{.Version}}' github.com/wailsapp/go-webview2), upstream: $(go list -m -f '{{.Version}}' github.com/wailsapp/go-webview2@latest 2>/dev/null || echo '?')"

echo "== anacrolix/torrent (an untagged upstream commit until a release after v1.61.0: newer commits aren't listed above; move to a tagged release when there is one)"
echo "  ours: $(go list -m -f '{{.Version}}' github.com/anacrolix/torrent), latest tag: $(go list -m -f '{{.Version}}' github.com/anacrolix/torrent@latest 2>/dev/null || echo '?')"

echo "== Frontend (npm)"
(cd frontend && npm outdated) || true
echo "(npm outdated lists nothing when all are current)"

n=$(go list -m -u -f '{{if and .Indirect .Update}}x{{end}}' all | grep -c x)
echo "== $n indirect Go modules have updates (they come with the libraries above; not listed)"
