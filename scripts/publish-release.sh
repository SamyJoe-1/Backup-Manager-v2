#!/usr/bin/env bash
# Publish dist/ (made by scripts/release.sh) as the GitHub release v$(cat VERSION).
# Needs GITHUB_TOKEN that may write to the repository. BM_REPO overrides owner/name.
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="${BM_REPO:-SamyJoe-1/Backup-Manager-v2}"
TAG="v$(cat VERSION)"
: "${GITHUB_TOKEN:?set GITHUB_TOKEN}"
api() { curl -fsS -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github+json" "$@"; }
[ -f dist/SHA256SUMS ] || { echo "run scripts/release.sh first" >&2; exit 1; }
if api "https://api.github.com/repos/$REPO/releases/tags/$TAG" -o /tmp/bm-rel.json 2>/dev/null; then
  echo "release $TAG exists: replacing its files"
else
  api -X POST "https://api.github.com/repos/$REPO/releases" -o /tmp/bm-rel.json \
    -d "{\"tag_name\":\"$TAG\",\"name\":\"$TAG\",\"target_commitish\":\"$(git rev-parse HEAD)\",\"body\":\"Install: see docs/INSTALL.md\"}"
fi
id="$(sed -n 's/^  "id": \([0-9]*\),*/\1/p' /tmp/bm-rel.json | head -1)"
[ -n "$id" ] || id="$(tr ',' '\n' </tmp/bm-rel.json | sed -n 's/^{*"id": *\([0-9]*\)$/\1/p' | head -1)"
# remove files of the same name, then upload
tr ',' '\n' </tmp/bm-rel.json | awk 'match($0,/"url": *"[^"]*\/releases\/assets\/[0-9]+"/){u=substr($0,RSTART,RLENGTH);sub(/^"url": *"/,"",u);sub(/"$/,"",u)} match($0,/"name": *"[^"]*"/){n=substr($0,RSTART,RLENGTH);sub(/^"name": *"/,"",n);sub(/"$/,"",n);if(u!=""){print u" "n;u=""}}' \
  | while read -r url name; do case "$name" in backup-manager-linux-*|SHA256SUMS|VERSION) api -X DELETE "$url" >/dev/null ;; esac; done
for f in dist/backup-manager-linux-amd64 dist/backup-manager-linux-arm64 dist/SHA256SUMS dist/VERSION; do
  echo "uploading $(basename "$f")"
  api -X POST -H "Content-Type: application/octet-stream" --data-binary @"$f" \
    "https://uploads.github.com/repos/$REPO/releases/$id/assets?name=$(basename "$f")" >/dev/null
done
echo "published $TAG"
