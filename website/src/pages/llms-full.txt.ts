import type { APIRoute } from 'astro';
import { getCollection } from 'astro:content';
import { entryTitle, published } from '../lib/entries';

const SITE = 'https://contenox.com';

export const GET: APIRoute = async () => {
  const docs = (await getCollection('docs', published)).sort((a, b) => a.id.localeCompare(b.id));

  const out: string[] = [
    '# contenox — full documentation',
    '',
    '> Guardrails for AI agents. One file in your repository declares what may run, what needs a human, and what starts work; the task engine enforces it before every tool call. Apache-2.0, one binary, no account, on your own machine.',
    '',
    `Generated from ${SITE}. Every published documentation page follows, in path order, each preceded by its canonical URL. The curated index is at ${SITE}/llms.txt; the machine-readable formats are at ${SITE}/schema/.`,
    '',
    '---',
    '',
  ];

  for (const entry of docs) {
    out.push(
      `# ${entryTitle(entry)}`,
      '',
      `Source: ${SITE}/docs/${entry.id}/`,
      '',
      entry.body?.trim() ?? '',
      '',
      '---',
      '',
    );
  }

  return new Response(out.join('\n'), {
    headers: { 'content-type': 'text/plain; charset=utf-8' },
  });
};
