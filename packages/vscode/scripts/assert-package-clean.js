#!/usr/bin/env node

const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const expectedID =
  process.env.CONTENOX_VSCODE_EXTENSION_ID || "contenox.contenox-runtime";
const expectedTarget = process.env.CONTENOX_VSCODE_TARGET || "";
const allowProposed = process.env.CONTENOX_ALLOW_PROPOSED === "1";
const allowUntargeted = process.env.CONTENOX_ALLOW_UNTARGETED === "1";
const files = process.argv.slice(2);

if (files.length === 0) {
  console.error(
    "usage: node scripts/assert-package-clean.js <file.vsix> [...]",
  );
  process.exit(2);
}

let failed = false;
for (const file of files) {
  try {
    checkVSIX(file);
    console.log(`Package check passed: ${file}`);
  } catch (error) {
    failed = true;
    console.error(`Package check failed: ${file}`);
    console.error(error instanceof Error ? error.message : String(error));
  }
}

if (failed) {
  process.exit(1);
}

function checkVSIX(file) {
  if (!fs.existsSync(file)) {
    throw new Error(`missing VSIX: ${file}`);
  }

  const entries = unzipList(file);
  const entrySet = new Set(entries);
  const required = [
    "extension/package.json",
    "extension/readme.md",
    "extension/changelog.md",
    "extension/SECURITY.md",
    "extension/SUPPORT.md",
    "extension/LICENSE.txt",
    "extension/media/contenox-icon.png",
    "extension/media/chat/webview.js",
    "extension/media/chat/webview.css",
    "extension/dist/approval/nativeTool.js",
  ];
  for (const entry of required) {
    if (!entrySet.has(entry)) {
      throw new Error(`required package file is missing: ${entry}`);
    }
  }

  const bins = entries.filter(
    (entry) =>
      entry === "extension/bin/contenox" ||
      entry === "extension/bin/contenox.exe",
  );
  if (bins.length !== 1) {
    throw new Error(
      `expected exactly one bundled binary, got ${bins.length}: ${bins.join(", ")}`,
    );
  }

  const forbidden = [
    /^extension\/src\//,
    /^extension\/node_modules\//,
    /^extension\/scripts\//,
    /^extension\/\.github\//,
    /^extension\/.*\.map$/,
    // /^extension\/.*\.svg$/i,
    /^extension\/.*\.vsix$/i,
    /^extension\/.*\.env($|\.)/i,
  ];
  for (const entry of entries) {
    if (forbidden.some((pattern) => pattern.test(entry))) {
      throw new Error(`forbidden package file included: ${entry}`);
    }
  }
  if (entrySet.has("extension/dist/chat/ChatPanel.js")) {
    throw new Error("stale ChatPanel.js must not be packaged");
  }
  for (const entry of entries.filter(
    (candidate) =>
      candidate.startsWith("extension/dist/") && candidate.endsWith(".js"),
  )) {
    const text = unzipText(file, entry);
    if (text.includes("Contenox requests permission:")) {
      throw new Error(`legacy notification approval path included: ${entry}`);
    }
  }

  const pkg = JSON.parse(unzipText(file, "extension/package.json"));
  const manifest = unzipText(file, "extension.vsixmanifest");
  const target = manifestTarget(manifest);
  if (expectedTarget && target !== expectedTarget) {
    throw new Error(
      `VSIX target is ${JSON.stringify(target)}, expected ${JSON.stringify(expectedTarget)}`,
    );
  }
  if (!target && !allowUntargeted) {
    throw new Error(
      "VSIX is missing TargetPlatform; native Contenox packages must be built with vsce --target",
    );
  }
  if (target) {
    checkBinaryNameForTarget(target, bins[0]);
  }

  const id = `${pkg.publisher}.${pkg.name}`;
  if (id !== expectedID) {
    throw new Error(`extension id is ${id}, expected ${expectedID}`);
  }
  if (pkg.private !== undefined) {
    throw new Error(
      "package.json must not include private for Marketplace packages",
    );
  }
  if (pkg.pricing !== "Free") {
    throw new Error(
      `package.json pricing must be Free, got ${JSON.stringify(pkg.pricing)}`,
    );
  }
  if (
    !pkg.homepage ||
    !String(pkg.homepage).startsWith("https://contenox.com")
  ) {
    throw new Error(
      `package.json homepage should point at https://contenox.com, got ${JSON.stringify(pkg.homepage)}`,
    );
  }
  if (!pkg.icon || !String(pkg.icon).endsWith(".png")) {
    throw new Error(
      `package.json icon must be a PNG, got ${JSON.stringify(pkg.icon)}`,
    );
  }
  if (
    !allowProposed &&
    Array.isArray(pkg.enabledApiProposals) &&
    pkg.enabledApiProposals.length > 0
  ) {
    throw new Error(
      "stable Marketplace package must not include enabledApiProposals",
    );
  }
  const tools = pkg.contributes?.languageModelTools;
  if (
    !Array.isArray(tools) ||
    !tools.some((tool) => tool?.name === "approve_contenox_tool_call")
  ) {
    throw new Error(
      "package must contribute the native Contenox approval language model tool",
    );
  }
}

function manifestTarget(manifest) {
  const match = /\bTargetPlatform="([^"]+)"/.exec(manifest);
  return match ? match[1] : "";
}

function checkBinaryNameForTarget(target, bin) {
  const wantsExe = target.startsWith("win32-");
  if (wantsExe && bin !== "extension/bin/contenox.exe") {
    throw new Error(
      `target ${target} must include extension/bin/contenox.exe, got ${bin}`,
    );
  }
  if (!wantsExe && bin !== "extension/bin/contenox") {
    throw new Error(
      `target ${target} must include extension/bin/contenox, got ${bin}`,
    );
  }
}

function unzipList(file) {
  const result = spawnSync("unzip", ["-Z1", file], { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(result.stderr || `unzip -Z1 failed for ${file}`);
  }
  return result.stdout
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
}

function unzipText(file, entry) {
  const result = spawnSync("unzip", ["-p", file, entry], { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(result.stderr || `unzip -p failed for ${file}:${entry}`);
  }
  return result.stdout;
}
