// Phase 5 "walk-away" two-process acceptance test config, deliberately
// separate from .vscode-test.visual.js: it must run ONLY walkaway.vtest.js
// (not the whole Phase 1-4 visual suite) so scripts/run-walkaway-acceptance.sh
// can launch this twice -- once per phase, with a real `contenox approvals
// respond` and a full process teardown in between -- without re-running an
// unrelated 10+ minute suite each time. Shares the visual suite's
// user-data-dir on purpose: "reopen VS Code" means the same profile/window
// coming back, not a fresh one.
const path = require("node:path");
const { defineConfig } = require("@vscode/test-cli");

const userDataDir = path.join(__dirname, ".vscode-test", "visual-user-data");
process.env.CONTENOX_VISUAL_USER_DATA = userDataDir;

module.exports = defineConfig({
  files: "dist/test/walkaway.vtest.js",
  workspaceFolder: path.join(__dirname, "test", "fixtures", "smoke-workspace"),
  launchArgs: [
    "--disable-extensions",
    `--user-data-dir=${userDataDir}`,
    "--remote-debugging-port=0",
  ],
  mocha: {
    timeout: 300000,
  },
});
