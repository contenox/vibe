const path = require("node:path");
const { defineConfig } = require("@vscode/test-cli");

module.exports = defineConfig({
  files: "dist/test/**/*.test.js",
  workspaceFolder: path.join(__dirname, "test", "fixtures", "smoke-workspace"),
  launchArgs: [
    "--disable-extensions",
    "--user-data-dir",
    path.join(__dirname, ".vscode-test", "user-data"),
  ],
  mocha: {
    timeout: 20000,
  },
});
