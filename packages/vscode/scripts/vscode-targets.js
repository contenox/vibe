const supportedTargets = {
  "linux-x64": { goos: "linux", goarch: "amd64", executable: "contenox" },
  "linux-arm64": { goos: "linux", goarch: "arm64", executable: "contenox" },
  "darwin-x64": { goos: "darwin", goarch: "amd64", executable: "contenox" },
  "darwin-arm64": { goos: "darwin", goarch: "arm64", executable: "contenox" },
  "win32-x64": { goos: "windows", goarch: "amd64", executable: "contenox.exe" },
};

function targetNames() {
  return Object.keys(supportedTargets);
}

function resolveTarget(target) {
  if (!target || !supportedTargets[target]) {
    throw new Error(`unsupported VS Code target ${JSON.stringify(target)}; expected one of ${targetNames().join(", ")}`);
  }
  return { name: target, ...supportedTargets[target] };
}

function targetFromHost(platform = process.platform, arch = process.arch) {
  const targetPlatform = platform === "win32" ? "win32" : platform;
  const targetArch = arch === "x64" ? "x64" : arch;
  return resolveTarget(`${targetPlatform}-${targetArch}`);
}

function targetFromGoEnv(goos, goarch) {
  const entry = Object.entries(supportedTargets).find(([, target]) => target.goos === goos && target.goarch === goarch);
  if (!entry) {
    throw new Error(`unsupported Go target GOOS=${goos || ""} GOARCH=${goarch || ""}; expected one of ${targetNames().join(", ")}`);
  }
  return resolveTarget(entry[0]);
}

function targetFromEnv(env = process.env) {
  const configured = env.CONTENOX_VSCODE_TARGET || env.VSCE_TARGET;
  if (configured) {
    return resolveTarget(configured);
  }
  if (env.GOOS || env.GOARCH) {
    if (!env.GOOS || !env.GOARCH) {
      throw new Error("GOOS and GOARCH must both be set when CONTENOX_VSCODE_TARGET is not set");
    }
    return targetFromGoEnv(env.GOOS, env.GOARCH);
  }
  return targetFromHost();
}

function assertGoEnvMatchesTarget(target, env = process.env) {
  if (env.GOOS && env.GOOS !== target.goos) {
    throw new Error(`GOOS=${env.GOOS} does not match ${target.name} (${target.goos})`);
  }
  if (env.GOARCH && env.GOARCH !== target.goarch) {
    throw new Error(`GOARCH=${env.GOARCH} does not match ${target.name} (${target.goarch})`);
  }
}

module.exports = {
  assertGoEnvMatchesTarget,
  resolveTarget,
  supportedTargets,
  targetFromEnv,
  targetFromGoEnv,
  targetFromHost,
  targetNames,
};
