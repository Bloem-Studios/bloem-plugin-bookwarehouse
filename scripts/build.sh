#!/bin/sh
# Reproducible raw executables; native targets also get extracted release metadata.
set -eu
cd "$(dirname "$0")/.."

GO=${GO:-go}
export GOTOOLCHAIN=go1.27.4 GOWORK=off GOENV=off GOFLAGS= CGO_ENABLED=0
export GOMAXPROCS=${GOMAXPROCS:-2} GOAMD64=v1 GOARM64=v8.0 GOEXPERIMENT=
export LC_ALL=C

host_os=$("$GO" env GOHOSTOS)
host_arch=$("$GO" env GOHOSTARCH)
if [ "$#" -eq 0 ]; then
    set -- "$host_os/$host_arch"
elif [ "$#" -eq 1 ] && [ "$1" = "--all" ]; then
    set -- linux/amd64 linux/arm64 darwin/arm64
fi

for target do
    case "$target" in
        linux/amd64|linux/arm64|darwin/arm64) ;;
        *) echo "unsupported target: $target" >&2; exit 2 ;;
    esac
    target_os=${target%/*}
    target_arch=${target#*/}
    output="dist/$target_os-$target_arch"
    mkdir -p "$output"
    # Old metadata must never accidentally accompany a newly built binary.
    rm -f "$output/manifest.json" "$output/SHA256SUMS" "$output/native-storage-approvals.example.json"
    GOOS="$target_os" GOARCH="$target_arch" "$GO" build \
        -mod=readonly -trimpath -buildvcs=false -p 2 \
        -ldflags="-s -w -buildid= -X main.version=0.4.0" \
        -o "$output/bloem-plugin-bookwarehouse" ./cmd/bloem-plugin-bookwarehouse
    if [ "$target_os/$target_arch" = "$host_os/$host_arch" ]; then
        python3 scripts/package.py "$output/bloem-plugin-bookwarehouse" "$target_os" "$target_arch"
    else
        echo "Built $output/bloem-plugin-bookwarehouse; extract metadata on $target_os/$target_arch:"
        echo "python3 scripts/package.py $output/bloem-plugin-bookwarehouse $target_os $target_arch"
    fi
done
