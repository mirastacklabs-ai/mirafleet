#!/usr/bin/env node
// Launcher for the user-installed `mirafleet` binary. The plugin ships no binary: this file finds
// `mirafleet` on PATH and runs it with the same arguments, no shell, stdio inherited.
// It downloads nothing and looks at no environment variable except PATH.
"use strict";

const { spawn } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const win = process.platform === "win32";

// File names to try for "mirafleet" in each PATH directory. On Windows only real executables
// qualify: a .cmd or .bat shim (npm) cannot be run without a shell.
const names = win ? ["mirafleet.exe"] : ["mirafleet"];

function find() {
  const dirs = (process.env.PATH || "").split(path.delimiter).filter(Boolean);
  for (const dir of dirs) {
    for (const name of names) {
      const d = dir.replace(/^"(.*)"$/, "$1");
      // Relative entries (".", "bin") would resolve against the project; never run those.
      if (!path.isAbsolute(d)) continue;
      const p = path.join(d, name);
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
    "mirafleet: binary not found on PATH. Install it (Homebrew, install.sh, install.ps1 or a " +
      "release archive; see the plugin README), then start Claude Code from a shell where " +
      "`mirafleet` is on PATH.\n"
  );
  process.exit(127);
}

// The hook and the MCP server each run behind their own launcher process, so os.Getppid() in the
// binary would differ between them; the binary keys its session handoff on Claude Code's PID
// instead. The child inherits this process's environment, with that one variable added.
process.env.MIRAFLEET_HARNESS_PID = String(process.ppid);
const child = spawn(bin, process.argv.slice(2), { stdio: "inherit", shell: false });

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
    // Still alive (signal handled or unsupported, e.g. on Windows): never exit 0 after a signal death.
    process.exit(128 + (os.constants.signals[signal] || 0));
  }
  process.exit(code === null ? 1 : code);
});
