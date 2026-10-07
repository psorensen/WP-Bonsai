# Releasing

Bonsai is published to npm as `wp-bonsai`, plus one package per platform that holds the binary:

| Package | Platform |
| --- | --- |
| `wp-bonsai-darwin-arm64` | macOS, Apple silicon |
| `wp-bonsai-darwin-x64` | macOS, Intel |
| `wp-bonsai-linux-x64` | Linux, x64 |
| `wp-bonsai-linux-arm64` | Linux, arm64 |

`wp-bonsai` lists the platform packages as optional dependencies. npm installs only the one that matches the machine. The launcher in `npm/wp-bonsai/bin/bonsai.js` finds that package and runs its binary.

## One-time setup

1. Create an npm account, or use an existing one, with two-factor authentication.
2. Create a granular access token that can publish `wp-bonsai` and `wp-bonsai-*`.
3. Add it to the GitHub repository as the secret `NPM_TOKEN`.

## Publishing a version

Push a version tag:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The `Release` workflow then:

1. Runs the tests and builds the binaries on native runners: macOS 14 builds both macOS packages, and Ubuntu 22.04 runners build the two Linux packages.
2. Publishes the four platform packages, then `wp-bonsai`, all at the tag's version, with npm provenance.

## Building locally

`scripts/build-npm.sh` builds the same packages into `dist/npm/`:

```sh
scripts/build-npm.sh 0.2.0-test darwin-arm64 darwin-x64 main
```

To try an install without publishing, pack the packages and install them into a throwaway prefix:

```sh
cd dist/npm
(cd wp-bonsai-darwin-arm64 && npm pack --pack-destination ..)
(cd wp-bonsai && npm pack --pack-destination ..)
npm install -g --prefix /tmp/bonsai-try ./wp-bonsai-darwin-arm64-0.2.0-test.tgz ./wp-bonsai-0.2.0-test.tgz
/tmp/bonsai-try/bin/bonsai version
```
