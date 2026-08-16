import { defineConfig } from "astro/config";
import tailwindcss from "@tailwindcss/vite";
import sitemap from "@astrojs/sitemap";

export default defineConfig({
  site: "https://contenox.com",
  output: "static",
  integrations: [
    sitemap({
      // Development docs (contributor docs and blueprint design records) stay
      // browsable and searchable on-site, but are kept out of the SEO surface.
      filter: (page) => !page.includes('/docs/development/'),
    }),
  ],
  markdown: {
    remarkPlugins: [(await import("./src/lib/remark-md-links.mjs")).default],
    shikiConfig: {
      themes: { light: "github-light", dark: "github-dark" },
      defaultColor: false,
    },
  },
  vite: {
    plugins: [tailwindcss()],
  },
  // contenox.com was heavily indexed as a Next.js app. Every route the old
  // site served (or redirected) must keep resolving on the static site: known
  // routes get explicit redirect pages here; everything else (tokened invites,
  // /bob/*, /api/*) is caught by the 404 page, which forwards to the homepage.
  redirects: {
    // 2026-07 docs restructure: guide/ was split into pillar sections.
    "/docs/guide/providers/anthropic": "/docs/integrations/providers/anthropic/",
    "/docs/guide/providers/bedrock": "/docs/integrations/providers/bedrock/",
    "/docs/guide/providers/gemini": "/docs/integrations/providers/gemini/",
    "/docs/guide/providers/mistral": "/docs/integrations/providers/mistral/",
    "/docs/guide/providers/ollama": "/docs/integrations/providers/ollama/",
    "/docs/guide/providers/openai": "/docs/integrations/providers/openai/",
    "/docs/guide/providers/openrouter": "/docs/integrations/providers/openrouter/",
    "/docs/guide/providers/vertex": "/docs/integrations/providers/vertex/",
    "/docs/guide/local-models": "/docs/rnd/modeld/",
    // The VS Code extension is retired again; both old URLs now land on its
    // lab retrospective, same as the local-models -> modeld redirect above.
    "/docs/guide/vscode-vscodium": "/docs/rnd/editor-agent/",
    "/docs/rnd/vscode-extension": "/docs/rnd/editor-agent/",
    "/docs/integrations/editors/vscode-vscodium": "/docs/rnd/editor-agent/",
    "/docs/guide/zed": "/docs/integrations/editors/zed/",
    "/docs/guide/jetbrains": "/docs/integrations/editors/jetbrains/",
    "/docs/guide/aionui": "/docs/integrations/editors/aionui/",
    "/docs/guide/openclaw": "/docs/integrations/editors/openclaw/",
    "/docs/guide/mcp": "/docs/integrations/tools/mcp/",
    // cookbook/ and stories/ merged into docs/use-cases/.
    "/cookbook": "/docs/use-cases/",
    "/stories": "/docs/use-cases/",
    "/cookbook/codebase-docs": "/docs/use-cases/",
    "/cookbook/git-devops": "/docs/use-cases/",
    "/cookbook/hubspot-mcp": "/docs/use-cases/hubspot-mcp/",
    "/cookbook/leads-to-hubspot": "/docs/use-cases/",
    "/cookbook/notion-mcp": "/docs/use-cases/notion-mcp/",
    "/cookbook/playwright-mcp": "/docs/use-cases/playwright-mcp/",
    "/cookbook/release-notes": "/docs/use-cases/",
    "/cookbook/stateful-agents-mcp": "/docs/use-cases/stateful-agents-mcp/",
    "/stories/any-api-as-a-tool": "/docs/use-cases/any-api-as-a-tool/",
    "/stories/authored-approval": "/docs/use-cases/authored-approval/",
    "/stories/moderation-gate": "/docs/use-cases/moderation-gate/",
    "/stories/multi-provider-fallback": "/docs/use-cases/multi-provider-fallback/",
    "/stories/nested-permission-bomb": "/docs/use-cases/nested-permission-bomb/",
    "/stories/openapi-subset": "/docs/use-cases/openapi-subset/",
    // chains/ was live at /docs/chains/ before its rename to specification/.
    "/docs/chains": "/docs/specification/",
    "/docs/chains/handlers": "/docs/specification/handlers/",
    "/docs/chains/transitions": "/docs/specification/transitions/",
    "/docs/chains/examples": "/docs/specification/examples/",
    // tools/ was live at /docs/tools/ before nesting under integrations/.
    "/docs/tools": "/docs/integrations/tools/",
    "/docs/tools/local": "/docs/integrations/tools/local/",
    "/docs/tools/remote": "/docs/integrations/tools/remote/",
    // 2026-08 agent-server reshape: contenox chat/run/index/search and the
    // one-shot CLI-filter use-cases built on `contenox run` are retired.
    "/docs/guide/search": "/docs/integrations/tools/mcp/",
    "/docs/use-cases/codebase-docs": "/docs/use-cases/",
    "/docs/use-cases/git-devops": "/docs/use-cases/",
    "/docs/use-cases/leads-to-hubspot": "/docs/use-cases/",
    "/docs/use-cases/release-notes": "/docs/use-cases/",
    // Old marketing/docs aliases the Next config carried.
    "/features": "/docs/guide/quickstart/",
    "/docs/beam": "/docs/rnd/beam-web/",
    "/docs/guide/introduction": "/docs/guide/quickstart/",
    // Retired EE/commerce surfaces.
    "/pricing": "/",
    "/services": "/",
    "/cloud": "/",
    "/login": "/",
    "/signup": "/",
    "/forgot-password": "/",
    "/reset-password": "/",
    "/invite": "/",
    "/admin": "/",
    "/admin/login": "/",
    "/admin/bob": "/",
    "/bob": "/",
    "/pilot": "/",
    "/pilot/managed-mcp": "/",
    "/pilot/success": "/",
    "/pilot/cancel": "/",
    // Pages that return with the content pass; keep the URLs alive meanwhile.
    "/about": "/",
  },
});
