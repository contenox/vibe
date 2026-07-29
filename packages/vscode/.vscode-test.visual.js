// Visual harness config, deliberately separate from .vscode-test.js.
//
// VS Code refuses to run extension tests while another instance holds the same
// user-data-dir, so this suite gets its own directory and its own file glob
// (*.vtest.js). `npm test` therefore never launches it, and the two can never
// collide with each other or with a developer's open editor.
const path = require("node:path");
const { defineConfig } = require("@vscode/test-cli");

// Port 0 = let Chromium pick a free one and write it to
// <user-data-dir>/DevToolsActivePort. Hardcoding loses: 9229 is Node's default
// inspector port and is routinely taken, and a busy port makes VS Code log
// "Cannot start http server for devtools" while the suite hangs waiting to
// attach. The extension host inherits this env var and reads the file.
const userDataDir = path.join(__dirname, ".vscode-test", "visual-user-data");
process.env.CONTENOX_VISUAL_USER_DATA = userDataDir;

module.exports = defineConfig({
  files: "dist/test/**/*.vtest.js",
  workspaceFolder: path.join(__dirname, "test", "fixtures", "smoke-workspace"),
  // Every flag uses the --flag=value form. test-cli treats a bare entry as a
  // folder path and reorders it to the end, so the two-entry form makes
  // --user-data-dir swallow whatever flag follows it.
  launchArgs: [
    "--disable-extensions",
    `--user-data-dir=${userDataDir}`,
    // The suite attaches to this same window over CDP for DOM assertions and
    // screenshots; without the port it can only take pictures.
    "--remote-debugging-port=0",
  ],
  mocha: {
    timeout: 180000,
  },
});
