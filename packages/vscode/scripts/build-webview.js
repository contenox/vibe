#!/usr/bin/env node

const fs = require("node:fs");
const path = require("node:path");
const esbuild = require("esbuild");
const postcss = require("postcss");
const tailwindcss = require("@tailwindcss/postcss");

const root = path.resolve(__dirname, "..");
const outDir = path.join(root, "media", "chat");

async function buildScript() {
  await esbuild.build({
    entryPoints: [path.join(root, "webview-src", "chat-entry.tsx")],
    bundle: true,
    outfile: path.join(outDir, "webview.js"),
    format: "iife",
    platform: "browser",
    target: "es2022",
    jsx: "automatic",
    sourcemap: true,
    minify: true,
    logLevel: "info",
    // Pin a single React copy: two copies null out the hooks dispatcher.
    alias: {
      react: path.join(root, "node_modules", "react"),
      "react-dom": path.join(root, "node_modules", "react-dom"),
      "lucide-react": path.join(root, "node_modules", "lucide-react"),
      "react-markdown": path.join(root, "node_modules", "react-markdown"),
      "remark-gfm": path.join(root, "node_modules", "remark-gfm"),
      clsx: path.join(root, "node_modules", "clsx"),
      "tailwind-merge": path.join(root, "node_modules", "tailwind-merge"),
    },
  });
}

async function buildStyles() {
  // webview.css imports the vendored ui theme, so one Tailwind pass sees both
  // the --color-* tokens and the sources using them. No prebuilt CSS needed.
  const cssEntry = path.join(root, "webview-src", "webview.css");
  const source = fs.readFileSync(cssEntry, "utf8");
  const result = await postcss([tailwindcss()]).process(source, {
    from: cssEntry,
    to: path.join(outDir, "webview.css"),
  });
  fs.writeFileSync(path.join(outDir, "webview.css"), result.css);
}

async function main() {
  fs.mkdirSync(outDir, { recursive: true });
  await buildScript();
  await buildStyles();
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
