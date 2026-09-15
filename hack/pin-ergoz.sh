#!/usr/bin/env bash
# Pin an ergoz release: vendor its packaged Helm chart under charts/ergoz and
# record the version and chart checksum in config/ergoz/release.json, which
# `sympozium install` verifies before installing it.
#
#   hack/pin-ergoz.sh v0.2.5
set -euo pipefail
version="${1:?ergoz release tag, e.g. v0.2.4}"
repo="$(cd "$(dirname "$0")/.." && pwd)"
chart="ergoz-${version#v}.tgz"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
gh release download "$version" -R sympozium-ai/ergoz -p "$chart" -p checksums.txt -D "$work"
(cd "$work" && grep " $chart\$" checksums.txt | sha256sum -c -)
rm -f "$repo"/charts/ergoz/ergoz-*.tgz
cp "$work/$chart" "$repo/charts/ergoz/$chart"
sha="$(sha256sum "$repo/charts/ergoz/$chart" | cut -d' ' -f1)"
printf '{\n  "version": "%s",\n  "chart": "%s",\n  "chartSHA256": "%s"\n}\n' "$version" "$chart" "$sha" >"$repo/config/ergoz/release.json"
cat "$repo/config/ergoz/release.json"
