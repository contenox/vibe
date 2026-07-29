#!/usr/bin/env node

const fs = require("node:fs");
const path = require("node:path");

const extensionRoot = path.resolve(__dirname, "..");
const repoRoot = path.resolve(extensionRoot, "..", "..");
const versionFile = path.join(repoRoot, "internal", "version", "version.txt");
const packagePath = path.join(extensionRoot, "package.json");
const lockPath = path.join(extensionRoot, "package-lock.json");
const readmePath = path.join(extensionRoot, "README.md");

function readJson(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function writeJson(file, value) {
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

function writeIfChanged(file, content) {
  const current = fs.existsSync(file) ? fs.readFileSync(file, "utf8") : "";
  if (current !== content) {
    fs.writeFileSync(file, content);
  }
}

function extensionVersionFromRuntimeVersion(rawVersion) {
  const trimmed = rawVersion.trim();
  if (!trimmed) {
    throw new Error(`${path.relative(repoRoot, versionFile)} is empty`);
  }

  const withoutPrefix = trimmed.replace(/^v/, "");
  const semverPattern =
    /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
  if (!semverPattern.test(withoutPrefix)) {
    throw new Error(
      `${path.relative(repoRoot, versionFile)} contains ${JSON.stringify(
        trimmed,
      )}; expected vX.Y.Z or valid SemVer so VS Code can package it`,
    );
  }
  return withoutPrefix;
}

function updatePackageJson(extensionVersion) {
  const pkg = readJson(packagePath);
  pkg.version = extensionVersion;

  if (!pkg.icon || typeof pkg.icon !== "string") {
    throw new Error("packages/vscode/package.json must declare an icon path");
  }

  const iconPath = path.resolve(extensionRoot, pkg.icon);
  if (!fs.existsSync(iconPath)) {
    throw new Error(`VS Code extension icon is missing: ${path.relative(extensionRoot, iconPath)}`);
  }

  writeJson(packagePath, pkg);
  return pkg;
}

function updatePackageLock(pkg) {
  if (!fs.existsSync(lockPath)) {
    return;
  }

  const lock = readJson(lockPath);
  lock.name = pkg.name;
  lock.version = pkg.version;
  lock.packages = lock.packages || {};
  lock.packages[""] = lock.packages[""] || {};
  lock.packages[""].name = pkg.name;
  lock.packages[""].version = pkg.version;
  writeJson(lockPath, lock);
}

function updateReadme(pkg) {
  if (!fs.existsSync(readmePath)) {
    return;
  }

  const expected = `${pkg.name}-${pkg.version}.vsix`;
  const current = fs.readFileSync(readmePath, "utf8");
  const updated = current.replace(/(?:contenox-runtime|contenox|runtime)(?:-vscode)?-[0-9A-Za-z.+-]+\.vsix/g, expected);
  writeIfChanged(readmePath, updated);
}

function removeStaleVsix(pkg) {
  const expected = `${pkg.name}-${pkg.version}.vsix`;
  const currentTargetPackage = new RegExp(`^${escapeRegExp(pkg.name)}-[a-z0-9]+-[a-z0-9]+-${escapeRegExp(pkg.version)}\\.vsix$`);
  for (const entry of fs.readdirSync(extensionRoot)) {
    if (!/^(?:contenox|runtime).*\.vsix$/.test(entry)) {
      continue;
    }
    if (entry === expected || currentTargetPackage.test(entry)) {
      continue;
    }
    fs.unlinkSync(path.join(extensionRoot, entry));
    console.log(`Removed stale VSIX: ${entry}`);
  }
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

const runtimeVersion = fs.readFileSync(versionFile, "utf8");
const extensionVersion = extensionVersionFromRuntimeVersion(runtimeVersion);
const pkg = updatePackageJson(extensionVersion);
updatePackageLock(pkg);
updateReadme(pkg);
removeStaleVsix(pkg);

console.log(
  `Synced VS Code package metadata: runtime ${runtimeVersion.trim()} -> extension ${pkg.version}; icon ${pkg.icon}`,
);
