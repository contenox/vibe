import type { APIRoute } from 'astro';
import { getCollection } from 'astro:content';
import { entryTitle, published } from '../lib/entries';

const SITE = 'https://contenox.com';

export const GET: APIRoute = async () => {
  const docs = (await getCollection('docs', published)).sort((a, b) => a.id.localeCompare(b.id));

  const out: string[] = [
    '# contenox — full documentation',
    '',
    '> An agent server. You don\'t build an agent, you declare one: an agent is files in `.contenox/` — chain files (`chain-agent-*.json`) declaring tasks, model routing, tools, retries and branching, and HITL policy files declaring what needs a human. Edit a file and the next invocation runs it: no build step, no plugin API, no release. Both kinds are JSON Schema–validated. contenox is a process you run and connect to, and the same chain runs unchanged in the terminal, headless in CI, inside an ACP editor, and as a unit the fleet dispatches. Every action is checked against your policy before it runs. Open source under Apache-2.0, one binary, no account, on your own machine, on any AI model you pick.',
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
