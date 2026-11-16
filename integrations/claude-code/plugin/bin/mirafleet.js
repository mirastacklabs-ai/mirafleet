#!/usr/bin/env node
// Launcher for the user-installed `mirafleet` binary. The plugin ships no binary: this file finds
// `mirafleet` on PATH and runs it with the same arguments, no shell, stdio inherited.
// It downloads nothing and looks at no environment variable except PATH.
"use strict";

const { spawn } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const win = process.platform === "win32";

// File names to try for "mirafleet" in each PATH directory. On Windows only real executables
// qualify: a .cmd or .bat shim (npm) cannot be run without a shell.
const names = win ? ["mirafleet.exe", "mirafleet.com"] : ["mirafleet"];

function find() {
  const dirs = (process.env.PATH || "").split(path.delimiter).filter(Boolean);
  for (const dir of dirs) {
    for (const name of names) {
      const p = path.join(dir.replace(/^"(.*)"$/, "$1"), name);
      try {
        if (!fs.statSync(p).isFile()) continue;
        if (!win) fs.accessSync(p, fs.constants.X_OK);
        return p;
      } catch {
        // not in this directory
      }
    }
  }
  return null;
}

const bin = find();
if (!bin) {
  process.stderr.write(
    "mirafleet: binary not found on PATH. Install it by one of:\n" +
      "  brew install mirastacklabs-ai/tap/mirafleet\n" +
      "  install.sh (macOS, Linux) or install.ps1 (Windows)\n" +
      "  a release archive from https://github.com/mirastacklabs-ai/mirafleet/releases\n" +
      "then start Claude Code from a shell where `mirafleet` is on PATH.\n"
  );
  process.exit(127);
}

// The environment is passed through unchanged except for one addition: Claude Code's PID. The hook
// and the MCP server each run behind their own launcher process, so os.Getppid() in the binary
// would differ between them; the binary keys its session handoff on this PID instead.
const env = { ...process.env, MIRAFLEET_HARNESS_PID: String(process.ppid) };
const child = spawn(bin, process.argv.slice(2), { stdio: "inherit", shell: false, env });

// Forward termination signals so the binary can shut down cleanly.
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => child.kill(sig));
}

child.on("error", (err) => {
  process.stderr.write(`mirafleet: cannot run ${bin}: ${err.message}\n`);
  process.exit(126);
});

child.on("exit", (code, signal) => {
  if (signal) {
    // Die from the same signal so the caller sees it.
    for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) process.removeAllListeners(sig);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code === null ? 1 : code);
});
