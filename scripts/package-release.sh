#!/usr/bin/env bash
set -euo pipefail

# Local release tooling only. Never installs, reconfigures or reloads a host.
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
go_bin=${GO:-go}
version=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["plugins"][0]["version"])' "$root/registry.json")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid release version' >&2; exit 1; }
output=${1:-"$root/dist"}
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
cache=${XDG_CACHE_HOME:-"$HOME/.cache"}
mkdir -p -- "$cache"
scratch=$(mktemp -d "$cache/openrouter-release.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

CGO_ENABLED=1 GOOS=linux GOARCH=amd64 "$go_bin" -C "$root" build -trimpath -buildmode=c-shared -o "$scratch/openrouter.so" .
nm -D "$scratch/openrouter.so" > "$scratch/symbols.txt"
grep -q ' T cliproxy_plugin_init$' "$scratch/symbols.txt"
# Refuse an artifact whose imported GLIBC versions exceed the supported floor.
objdump -T "$scratch/openrouter.so" > "$scratch/dynamic-symbols.txt"
python3 -I - "$scratch/dynamic-symbols.txt" <<'PY'
import pathlib, re, sys
text = pathlib.Path(sys.argv[1]).read_text()
versions = [tuple(map(int, version.split('.'))) for version in re.findall(r'GLIBC_([0-9.]+)', text)]
if 'GLIBC_PRIVATE' in text or not versions or max(versions) > (2, 34):
    raise SystemExit('Release must import only public GLIBC symbols at version 2.34 or older')
print('Maximum imported GLIBC version: ' + '.'.join(map(str, max(versions))))
PY
archive="openrouter_${version}_linux_amd64.zip"
# Generated C headers, tests and credentials never enter the archive.
zip -q -X -j "$scratch/$archive" "$scratch/openrouter.so" "$root/LICENSE"
(cd -- "$scratch" && sha256sum "$archive") > "$scratch/checksums.txt"
cp -- "$scratch/$archive" "$scratch/checksums.txt" "$output/"
printf 'Created %s and checksums.txt\n' "$archive"
