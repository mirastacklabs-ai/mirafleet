#!/usr/bin/env node
"use strict";
// mirafleet launcher: finds the platform package that npm installed as an optional dependency
// and runs its binary with the same arguments, stdio and exit code. No dependencies, no
// network, nothing written to stdout.

const path = require("path");
const fs = require("fs");
const os = require("os");
const { spawn } = require("child_process");

const SCOPE = "@mirastacklabs";
const PLATFORMS = {
  "linux-x64": "mirafleet-linux-x64",
  "linux-arm64": "mirafleet-linux-arm64",
  "darwin-x64": "mirafleet-darwin-x64",
  "darwin-arm64": "mirafleet-darwin-arm64",
  "win32-x64": "mirafleet-win32-x64",
  "win32-arm64": "mirafleet-win32-arm64",
};

// platformPackage returns the platform package name, or null when the platform is not supported.
function platformPackage(platform, arch) {
  const p = PLATFORMS[platform + "-" + arch];
  return p ? SCOPE + "/" + p : null;
}

function binaryName(platform) {
  return platform === "win32" ? "mirafleet.exe" : "mirafleet";
}

// resolveBinary returns {bin} or {error}. resolve defaults to require.resolve and is injectable.
function resolveBinary(platform, arch, resolve) {
  resolve = resolve || require.resolve;
  const pkg = platformPackage(platform, arch);
  if (!pkg) {
    return {
      error:
        "mirafleet: unsupported platform " + platform + "/" + arch +
        " (supported: " + Object.keys(PLATFORMS).join(", ") + ")",
    };
  }
  let manifest;
  try {
    manifest = resolve(pkg + "/package.json");
  } catch (e) {
    return {
      error:
        "mirafleet: the platform package " + pkg + " is not installed. " +
        "Reinstall without --omit=optional (or --no-optional), then try again.",
    };
  }
  return { bin: path.join(path.dirname(manifest), "bin", binaryName(platform)), pkg };
}

function main() {
  const r = resolveBinary(process.platform, process.arch);
  if (r.error) {
    process.stderr.write(r.error + "\n");
    process.exit(1);
  }
  if (process.platform !== "win32") {
    try {
      fs.chmodSync(r.bin, 0o755); // best effort: some installers drop the exec bit
    } catch (e) {
      /* read-only install: the exec bit is normally already set */
    }
  }
  const child = spawn(r.bin, process.argv.slice(2), { stdio: "inherit" });
  const forward = (sig) => () => {
    try {
      child.kill(sig);
    } catch (e) {
      /* already gone */
    }
  };
  for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(sig, forward(sig));
  }
  child.on("error", (e) => {
    process.stderr.write("mirafleet: cannot run " + r.bin + " (" + r.pkg + "): " + e.message + "\n");
    process.exit(1);
  });
  child.on("exit", (code, signal) => {
    if (signal) {
      const n = os.constants.signals[signal];
      process.exit(n ? 128 + n : 1);
    }
    process.exit(code === null ? 1 : code);
  });
}

if (require.main === module) {
  main();
} else {
  module.exports = { platformPackage, binaryName, resolveBinary, PLATFORMS };
}
