"use strict";
const test = require("node:test");
const assert = require("node:assert");
const path = require("path");
const { platformPackage, binaryName, resolveBinary, PLATFORMS } = require("../bin/mirafleet.js");

test("maps every supported platform to a scoped package", () => {
  for (const key of Object.keys(PLATFORMS)) {
    const [plat, arch] = key.split("-");
    assert.strictEqual(platformPackage(plat, arch), "@mirastacklabs/mirafleet-" + key);
  }
  assert.strictEqual(Object.keys(PLATFORMS).length, 6);
});

test("unsupported platform is named in the error", () => {
  assert.strictEqual(platformPackage("freebsd", "x64"), null);
  const r = resolveBinary("freebsd", "x64");
  assert.match(r.error, /unsupported platform freebsd\/x64/);
});

test("missing platform package names the package", () => {
  const r = resolveBinary("linux", "x64", () => {
    throw new Error("not found");
  });
  assert.match(r.error, /@mirastacklabs\/mirafleet-linux-x64 is not installed/);
});

test("resolves the binary next to the platform manifest", () => {
  const r = resolveBinary("win32", "arm64", (id) => path.join("/x", "node_modules", id));
  assert.strictEqual(r.bin, path.join("/x/node_modules/@mirastacklabs/mirafleet-win32-arm64", "bin", "mirafleet.exe"));
  assert.strictEqual(binaryName("linux"), "mirafleet");
});
