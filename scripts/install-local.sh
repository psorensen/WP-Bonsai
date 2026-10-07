#!/usr/bin/env bash
# Builds bonsai from this checkout and installs it globally with npm, the
# same way a published release installs. After it finishes, run:
#
#   bonsai setup
#   bonsai prod.sql.gz
#
# Needs Go 1.27 or later, a C compiler (Xcode Command Line Tools on macOS,
# gcc on Linux), and Node.js 18 or later. Set BONSAI_NPM_PREFIX to install
# somewhere other than npm's global folder.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

fail() { echo "install-local: $*" >&2; exit 1; }

# The platform names match the npm packages.
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  platform=darwin-arm64 ;;
  Darwin-x86_64) platform=darwin-x64 ;;
  Linux-x86_64)  platform=linux-x64 ;;
  Linux-aarch64 | Linux-arm64) platform=linux-arm64 ;;
  *) fail "bonsai builds for macOS and Linux only, not $(uname -s) $(uname -m)" ;;
esac

command -v go >/dev/null || fail "Go is not installed. Install Go 1.27 or later: https://go.dev/dl/"
go_version="$(go env GOVERSION | sed 's/^go//')"
if [ "$(printf '%s\n1.27\n' "$go_version" | sort -V | head -1)" != "1.27" ]; then
  fail "Go $go_version is too old. Bonsai needs Go 1.27 or later."
fi
if [ "$platform" = darwin-arm64 ] || [ "$platform" = darwin-x64 ]; then
  command -v clang >/dev/null || fail "no C compiler. Install the Xcode Command Line Tools: xcode-select --install"
else
  command -v gcc >/dev/null || fail "no C compiler. Install gcc, for example: sudo apt install build-essential"
fi
command -v npm >/dev/null || fail "npm is not installed. Install Node.js 18 or later: https://nodejs.org"

# A semver version that names the commit this build came from.
sha="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
version="0.0.0-local.$sha"
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  version="$version.dirty"
fi

echo "Building bonsai $version for $platform. The first build downloads Go modules and takes a few minutes."
scripts/build-npm.sh "$version" "$platform" main

cd dist/npm
rm -f ./*.tgz
(cd "wp-bonsai-$platform" && npm pack --silent --pack-destination .. >/dev/null)
(cd wp-bonsai && npm pack --silent --pack-destination .. >/dev/null)

prefix=()
if [ -n "${BONSAI_NPM_PREFIX:-}" ]; then
  prefix=(--prefix "$BONSAI_NPM_PREFIX")
fi
echo "Installing with npm."
# The main package lists all four platform packages as optional. Only the
# local one is installed here; npm skips the others.
npm install -g "${prefix[@]}" --no-audit --no-fund \
  "./wp-bonsai-$platform-$version.tgz" "./wp-bonsai-$version.tgz"

bin="bonsai"
if [ -n "${BONSAI_NPM_PREFIX:-}" ]; then
  bin="$BONSAI_NPM_PREFIX/bin/bonsai"
fi
echo
"$bin" version
cat <<EOF

Installed. Next:
  bonsai setup          check Docker and prepare the sandbox image (once)
  bonsai prod.sql.gz    shrink a dump

To uninstall: npm uninstall -g wp-bonsai wp-bonsai-$platform
EOF
