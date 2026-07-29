#!/usr/bin/env node

// Bundles the extension host (CommonJS, platform node) with esbuild -- the
// same tool build-webview.js already uses for the webview bundle. Needed
// because @agentclientprotocol/sdk is ESM-only while the extension host is
// CommonJS; esbuild lowers the ESM dependency to CJS at build time instead
// of relying on Node's runtime ESM/CJS interop. See
// docs/development/internal/acp-client-ts-spike.md.
//
// Must run after `tsc -p .` (compile) in every pipeline that calls it, since
// both write dist/extension.js -- this step's output is the one that ships.

const path = require("node:path");
const esbuild = require("esbuild");

const root = path.resolve(__dirname, "..");
const isProd = process.env.NODE_ENV === "production";

async function main() {
  await esbuild.build({
    entryPoints: [path.join(root, "src", "extension.ts")],
    bundle: true,
    outfile: path.join(root, "dist", "extension.js"),
    format: "cjs",
    platform: "node",
    target: "node18",
    // Never bundle the vscode module: it's supplied by the extension host,
    // not resolvable as a real package.
    external: ["vscode"],
    sourcemap: !isProd,
    minify: isProd,
    logLevel: "info",
  });
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
