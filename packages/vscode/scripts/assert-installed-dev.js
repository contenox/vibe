#!/usr/bin/env node

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const extensionID = process.env.CONTENOX_VSCODE_EXTENSION_ID || "contenox.contenox-runtime";
const vscodeCli = process.env.VSCODE_CLI || "code";
const version = process.argv[2];

if (!version) {
  console.error("usage: node scripts/assert-installed-dev.js <version>");
  process.exit(2);
}

const extensionRoot = process.env.VSCODE_EXTENSIONS_DIR || defaultExtensionsDir(vscodeCli);
const extensionDir = path.join(extensionRoot, `${extensionID}-${version}`);
const checks = [
  ["package.json", "file"],
  ["dist/extension.js", "file"],
  ["dist/chat/SessionTreeProvider.js", "file"],
  ["dist/config/RuntimeControlsView.js", "file"],
  ["dist/approval/nativeTool.js", "file"],
  ["bin/contenox", "file"],
];

for (const [entry] of checks) {
  const file = path.join(extensionDir, entry);
  if (!fs.existsSync(file)) {
    fail(`installed extension is missing ${entry}: ${file}`);
  }
}

const pkg = readJson(path.join(extensionDir, "package.json"));
if (`${pkg.publisher}.${pkg.name}` !== extensionID) {
  fail(`installed extension id is ${pkg.publisher}.${pkg.name}, expected ${extensionID}`);
}
if (pkg.version !== version) {
  fail(`installed extension version is ${pkg.version}, expected ${version}`);
}
if (!pkg.contributes?.views?.contenox?.some((view) => view.id === "contenox.controls")) {
  fail("installed package does not contribute the Contenox Runtime view");
}
if (!pkg.contributes?.views?.contenox?.some((view) => view.id === "contenox.sessions")) {
  fail("installed package does not contribute the Contenox Sessions view");
}

// Verify key feature strings. Runtime config is accessible via chat header picker
// (ChatWebviewViewProvider) and the sidebar Runtime panel (RuntimeControlsView).
const sessionTree = readText(path.join(extensionDir, "dist/chat/SessionTreeProvider.js"));
if (!sessionTree.includes("SessionTreeProvider") || !sessionTree.includes("No sessions yet. Start chatting")) {
  fail("installed SessionTreeProvider.js appears to be missing or stale");
}
const chatWebview = readText(path.join(extensionDir, "dist/chat/ChatWebviewViewProvider.js"));
for (const marker of ["Provider", "Model", "Thinking level", "HITL policy", "contenox.selectHitlPolicy"]) {
  if (!chatWebview.includes(marker)) {
    fail(`installed ChatWebviewViewProvider.js is missing marker ${JSON.stringify(marker)}`);
  }
}
const controls = readText(path.join(extensionDir, "dist/config/RuntimeControlsView.js"));
for (const marker of ["Provider", "Model", "Thinking", "HITL Policy"]) {
  if (!controls.includes(marker)) {
    fail(`installed RuntimeControlsView.js is missing marker ${JSON.stringify(marker)}`);
  }
}
if (fs.existsSync(path.join(extensionDir, "dist/chat/ChatPanel.js"))) {
  fail("installed extension still contains stale dist/chat/ChatPanel.js");
}

const approvalTool = readText(path.join(extensionDir, "dist/approval/nativeTool.js"));
for (const marker of ["approval.native.payload", "approval.native.payload.missing_details"]) {
  if (!approvalTool.includes(marker)) {
    fail(`installed nativeTool.js is missing marker ${JSON.stringify(marker)}`);
  }
}
if (approvalTool.includes("Contenox requests permission:")) {
  fail("installed extension still contains legacy notification approval text");
}

console.log(`Installed extension verified: ${extensionDir}`);
console.log("Reload Window required before VS Code uses this extension build.");

function defaultExtensionsDir(cli) {
  const command = path.basename(cli).toLowerCase();
  if (command.includes("insiders")) {
    return path.join(os.homedir(), ".vscode-insiders", "extensions");
  }
  return path.join(os.homedir(), ".vscode", "extensions");
}

function readJson(file) {
  return JSON.parse(readText(file));
}

function readText(file) {
  return fs.readFileSync(file, "utf8");
}

function fail(message) {
  console.error(message);
  process.exit(1);
}
