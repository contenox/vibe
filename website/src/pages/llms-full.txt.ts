import type { APIRoute } from 'astro';
import { getCollection } from 'astro:content';
import { entryTitle, published } from '../lib/entries';
import { PRODUCT_SHAPES, PRODUCT_SUMMARY } from '../lib/summary';

const SITE = 'https://contenox.com';

export const GET: APIRoute = async () => {
  const docs = (await getCollection('docs', published)).sort((a, b) => a.id.localeCompare(b.id));

  const out: string[] = [
    '# contenox — full documentation',
    '',
    `> ${PRODUCT_SUMMARY}`,
    '',
    PRODUCT_SHAPES,
    '',
    `Generated from ${SITE}. Every published documentation page follows, in path order, each preceded by its canonical URL — including the contributor docs under development/ and the lab notes under rnd/. The curated index is at ${SITE}/llms.txt; the machine-readable formats are at ${SITE}/schema/.`,
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
