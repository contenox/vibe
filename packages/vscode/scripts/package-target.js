#!/usr/bin/env node

const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { targetFromEnv } = require("./vscode-targets");

const extensionRoot = path.resolve(__dirname, "..");
const packagePath = path.join(extensionRoot, "package.json");
const target = targetFromEnv();

run(process.execPath, [path.join("scripts", "sync-package-metadata.js")]);
run(npx(), ["tsc", "-p", "."], {}, true);
// Must run after tsc: both write dist/extension.js, and this bundle is the
// one that ships (tsc alone leaves an unbundled require() of the ESM-only
// @agentclientprotocol/sdk, which would crash in a packaged VSIX).
run(process.execPath, [path.join("scripts", "build-extension.js")], {
  NODE_ENV: "production",
});
run(process.execPath, [path.join("scripts", "build-webview.js")]);
run(process.execPath, [path.join("scripts", "build-binary.js")], {
  CONTENOX_VSCODE_TARGET: target.name,
});

const pkg = JSON.parse(fs.readFileSync(packagePath, "utf8"));
const out =
  process.env.CONTENOX_VSCODE_OUT ||
  path.join(extensionRoot, "artifacts", `${pkg.name}-${target.name}-${pkg.version}.vsix`);
fs.mkdirSync(path.dirname(out), { recursive: true });
fs.rmSync(out, { force: true });

const vsceArgs = [
  "vsce",
  "package",
  "--target",
  target.name,
  "--no-dependencies",
  "--no-yarn",
  "--out",
  out,
];
if (process.env.CONTENOX_VSCODE_SKIP_VSCE_SECRET_SCAN === "1") {
  vsceArgs.splice(vsceArgs.indexOf("--out"), 0, "--allow-package-all-secrets", "--allow-package-env-file");
}

// vsce always re-runs "vscode:prepublish" (sync:metadata && compile &&
// build:extension) before zipping, which re-emits dist/extension.js one more
// time -- so NODE_ENV=production has to reach that nested invocation too, or
// the shipped bundle silently reverts to the unminified dev build.
run(npx(), vsceArgs, { NODE_ENV: "production" });
console.log(`Built ${target.name} VS Code extension: ${path.relative(extensionRoot, out)}`);

function run(command, args, extraEnv = {}) {
  const result = spawnSync(command, args, {
    cwd: extensionRoot,
    env: {
      ...process.env,
      ...extraEnv,
    },
    stdio: "inherit",
  });
  if (result.status !== 0) {
    process.exit(result.status ?? 1);
  }
}

function npx() {
  return process.platform === "win32" ? "npx.cmd" : "npx";
}
