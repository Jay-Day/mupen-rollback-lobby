#!/usr/bin/env bash
# Cross-compile rmgk-lobby for Windows and Linux from one host.
#
# Usage:
#   ./build.sh                       # builds both platforms (default)
#   ./build.sh --os linux            # Linux only
#   ./build.sh --os windows          # Windows only
#   ./build.sh --arch arm64          # build for ARM64 instead of amd64
#   ./build.sh --no-strip            # keep debug symbols
#
# Output: bin/<os>/<binary>[.exe]
# CGO is force-disabled — produces static binaries, no glibc dependency.

set -euo pipefail
cd "$(dirname "$0")"

os_filter="all"
arch="amd64"
no_strip=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --os)        os_filter="$2"; shift 2 ;;
        --arch)      arch="$2";      shift 2 ;;
        --no-strip)  no_strip=1;     shift ;;
        -h|--help)
            sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "unknown arg: $1" >&2
            exit 2
            ;;
    esac
done

case "$os_filter" in
    all|windows|linux) ;;
    *) echo "--os must be one of: all, windows, linux" >&2; exit 2 ;;
esac
case "$arch" in
    amd64|arm64) ;;
    *) echo "--arch must be one of: amd64, arm64" >&2; exit 2 ;;
esac

binaries=("lobby-server" "smoke-client")
ldflags=""
if [[ $no_strip -eq 0 ]]; then
    ldflags="-s -w"
fi

targets=()
[[ "$os_filter" == "all" || "$os_filter" == "windows" ]] && targets+=("windows:.exe")
[[ "$os_filter" == "all" || "$os_filter" == "linux"   ]] && targets+=("linux:")

for target in "${targets[@]}"; do
    goos="${target%:*}"
    suffix="${target#*:}"
    outdir="bin/$goos"
    mkdir -p "$outdir"

    for bin in "${binaries[@]}"; do
        outpath="$outdir/$bin$suffix"
        echo "→ $goos/$arch  $outpath"

        build_args=(build -trimpath)
        [[ -n "$ldflags" ]] && build_args+=("-ldflags=$ldflags")
        build_args+=(-o "$outpath" "./cmd/$bin")

        GOOS="$goos" GOARCH="$arch" CGO_ENABLED=0 go "${build_args[@]}"
    done
done

echo
echo "Done. Artifacts:"
if command -v find >/dev/null 2>&1; then
    find bin -type f -exec ls -lh {} \;
else
    ls -lhR bin
fi
