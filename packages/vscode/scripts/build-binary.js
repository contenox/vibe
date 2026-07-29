#!/usr/bin/env node

const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { assertGoEnvMatchesTarget, targetFromEnv } = require("./vscode-targets");

const extensionRoot = path.resolve(__dirname, "..");
const repoRoot = path.resolve(extensionRoot, "..", "..");
const binDir = path.join(extensionRoot, "bin");
const target = targetFromEnv();
assertGoEnvMatchesTarget(target);

const executable = target.executable;
const output = path.join(binDir, executable);

fs.rmSync(binDir, { recursive: true, force: true });
fs.mkdirSync(binDir, { recursive: true });

const env = {
  ...process.env,
  CGO_ENABLED: process.env.CGO_ENABLED || "0",
  GOOS: target.goos,
  GOARCH: target.goarch,
};

const ldflags = ["-s", "-w"];
const version = process.env.CONTENOX_VERSION?.trim();
if (version) {
  ldflags.push(`-X github.com/contenox/contenox/internal/surfaces/contenoxcli.Version=${version}`);
}

const result = spawnSync("go", ["build", "-trimpath", "-ldflags", ldflags.join(" "), "-o", output, "./cmd/contenox"], {
  cwd: repoRoot,
  env,
  stdio: "inherit",
});

if (result.status !== 0) {
  process.exit(result.status ?? 1);
}

if (target.goos !== "windows" && os.platform() !== "win32") {
  fs.chmodSync(output, 0o755);
}

console.log(`Bundled Contenox binary for ${target.name}: ${path.relative(extensionRoot, output)}`);
