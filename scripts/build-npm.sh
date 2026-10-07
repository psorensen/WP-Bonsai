#!/usr/bin/env bash
# Builds the npm packages for one release into dist/npm/.
#
#   scripts/build-npm.sh 1.2.3 darwin-arm64 darwin-x64   # platform packages
#   scripts/build-npm.sh 1.2.3 linux-x64
#   scripts/build-npm.sh 1.2.3 main                       # the wp-bonsai package
#
# Each platform package holds one bonsai binary. DuckDB needs cgo, so each
# binary is built with a C toolchain for its platform: macOS builds both
# architectures with clang, and Linux builds on a native runner.
set -euo pipefail

version="${1:?usage: build-npm.sh <version> <platform|main>...}"
shift
[ $# -gt 0 ] || { echo "name at least one platform or main" >&2; exit 2; }

root="$(cd "$(dirname "$0")/.." && pwd)"
dist="$root/dist/npm"
mkdir -p "$dist"

build_platform() {
  local platform="$1" goos goarch cc os cpu
  case "$platform" in
    darwin-arm64) goos=darwin goarch=arm64 cc="clang -arch arm64"  os=darwin cpu=arm64 ;;
    darwin-x64)   goos=darwin goarch=amd64 cc="clang -arch x86_64" os=darwin cpu=x64 ;;
    linux-x64)    goos=linux  goarch=amd64 cc="gcc"                os=linux  cpu=x64 ;;
    linux-arm64)  goos=linux  goarch=arm64 cc="gcc"                os=linux  cpu=arm64 ;;
    *) echo "unknown platform: $platform" >&2; exit 2 ;;
  esac

  local pkg="wp-bonsai-$platform"
  local dir="$dist/$pkg"
  rm -rf "$dir"
  mkdir -p "$dir/bin"

  echo "building $pkg $version"
  (cd "$root" && CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" CC="$cc" \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$dir/bin/bonsai" ./cmd/bonsai)
  chmod 755 "$dir/bin/bonsai"

  cat > "$dir/package.json" <<EOF
{
  "name": "$pkg",
  "version": "$version",
  "description": "The bonsai binary for $platform. Install wp-bonsai instead.",
  "repository": { "type": "git", "url": "git+https://github.com/psorensen/WP-Bonsai.git" },
  "license": "UNLICENSED",
  "os": ["$os"],
  "cpu": ["$cpu"],
  "files": ["bin/bonsai"]
}
EOF
}

build_main() {
  local dir="$dist/wp-bonsai"
  rm -rf "$dir"
  mkdir -p "$dir/bin"
  cp "$root/npm/wp-bonsai/bin/bonsai.js" "$dir/bin/"
  chmod 755 "$dir/bin/bonsai.js"
  cp "$root/README.md" "$dir/"
  # Set the package version and pin every platform package to it.
  node -e '
    const fs = require("fs");
    const [src, dst, v] = process.argv.slice(1);
    const p = JSON.parse(fs.readFileSync(src, "utf8"));
    p.version = v;
    for (const k of Object.keys(p.optionalDependencies)) p.optionalDependencies[k] = v;
    fs.writeFileSync(dst, JSON.stringify(p, null, 2) + "\n");
  ' "$root/npm/wp-bonsai/package.json" "$dir/package.json" "$version"
  echo "built wp-bonsai $version"
}

for target in "$@"; do
  if [ "$target" = main ]; then build_main; else build_platform "$target"; fi
done
