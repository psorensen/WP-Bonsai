#!/usr/bin/env node
// Launcher for the bonsai binary. npm installs exactly one platform package
// next to this one, chosen by its "os" and "cpu" fields. This script finds
// that package's binary and runs it with the same arguments.
"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");

const platforms = {
  "darwin-arm64": "wp-bonsai-darwin-arm64",
  "darwin-x64": "wp-bonsai-darwin-x64",
  "linux-x64": "wp-bonsai-linux-x64",
  "linux-arm64": "wp-bonsai-linux-arm64",
};

const key = `${process.platform}-${process.arch}`;
const pkg = platforms[key];
if (!pkg) {
  console.error(
    `wp-bonsai: there is no build for ${key}. Builds exist for macOS (Apple silicon and Intel) and Linux (x64 and arm64).`
  );
  process.exit(1);
}

let bin;
try {
  bin = require.resolve(`${pkg}/bin/bonsai`);
} catch {
  console.error(
    `wp-bonsai: the package ${pkg} is missing. Reinstall with: npm install -g wp-bonsai\n` +
      "If you installed with --no-optional or --omit=optional, install without it."
  );
  process.exit(1);
}

// Some npm versions drop the executable bit. Restore it when we can.
try {
  fs.accessSync(bin, fs.constants.X_OK);
} catch {
  try {
    fs.chmodSync(bin, 0o755);
  } catch {
    // Not writable, for example a global install owned by root. Try anyway.
  }
}

// Ctrl-C reaches bonsai directly, because it shares this terminal. Ignore it
// here, so bonsai can stop its Docker sandbox and clean up before exiting.
process.on("SIGINT", () => {});
process.on("SIGTERM", () => {});

const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`wp-bonsai: ${result.error.message}`);
  process.exit(1);
}
if (result.signal) {
  process.kill(process.pid, result.signal);
}
process.exit(result.status === null ? 1 : result.status);
