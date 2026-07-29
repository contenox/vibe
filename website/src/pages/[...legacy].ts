import type { APIRoute } from 'astro';
import { getCollection } from 'astro:content';

// The previous contenox.com generations were indexed (and scraped) with
// `.html`-suffixed URLs. Emit a real redirect page for every such URL so
// legacy links resolve with a 200 + canonical instead of a 404.
// Entries whose id ends in `index` are skipped: their `.html` path is the
// file Astro already writes for the directory route.

type Target = { legacy: string; target: string };

export async function getStaticPaths() {
  const paths: { params: { legacy: string }; props: { target: string } }[] = [];
  const add = (legacy: string, target: string) => {
    paths.push({ params: { legacy }, props: { target } });
  };

  add('docs.html', '/docs/');
  add('cookbook.html', '/docs/use-cases/');
  add('stories.html', '/docs/use-cases/');
  add('de.html', '/de/');
  add('features.html', '/docs/guide/quickstart/');
  add('legal.html', '/legal/');
  for (const retired of [
    'pricing', 'services', 'cloud', 'login', 'signup', 'forgot-password',
    'reset-password', 'invite', 'admin', 'bob', 'pilot', 'about',
  ]) {
    add(`${retired}.html`, '/');
  }

  // Earlier research surfaces (Beam web UI, VS Code extension,
  // modeld local inference, the HTTP API layer). Their old doc URLs land on the
  // contenox lab pages that cover the same ground instead of
  // the bare docs index — better SEO continuity for links that already point
  // at them.
  const retiredDocs: Record<string, string> = {
    'guide/beam': 'rnd/beam-web',
    'development/beam-serve-auth': 'rnd/beam-web',
    'rnd/vscode-extension': 'integrations/editors/vscode-vscodium',
    'integrations/providers/modeld': 'rnd/modeld',
    'integrations/providers/modeld-architecture': 'rnd/modeld',
    'integrations/providers/local-models': 'rnd/modeld',
    'development/modeld-llama-backend': 'rnd/modeld',
    'development/modeld-local-inference-landscape': 'rnd/modeld',
    'development/modeld-release-runbook': 'rnd/modeld',
    'development/modeld-source-build': 'rnd/modeld',
    'development/api_spec_generation': 'rnd/api-layer',
  };
  for (const [id, target] of Object.entries(retiredDocs)) {
    add(`docs/${id}.html`, `/docs/${target}/`);
  }

  const skip = (id: string) =>
    id === 'index' || id.endsWith('/index') || id in retiredDocs;
  for (const entry of await getCollection('docs')) {
    if (!skip(entry.id)) add(`docs/${entry.id}.html`, `/docs/${entry.id}/`);
  }
  return paths;
}

export const GET: APIRoute<Target> = ({ props }) => {
  const target = new URL(props.target, 'https://contenox.com');
  const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Redirecting to ${target}</title>
<meta http-equiv="refresh" content="0;url=${props.target}">
<link rel="canonical" href="${target}">
<meta name="robots" content="noindex">
</head>
<body>
<p>This page has moved to <a href="${props.target}">${target}</a>.</p>
</body>
</html>
`;
  return new Response(html, { headers: { 'Content-Type': 'text/html; charset=utf-8' } });
};
