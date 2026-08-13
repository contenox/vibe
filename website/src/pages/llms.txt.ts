import type { APIRoute } from 'astro';
import { getCollection } from 'astro:content';
import { entryTitle, published } from '../lib/entries';

const SITE = 'https://contenox.com';

const SECTIONS: [string, string, string][] = [
  ['guide', 'Guides', 'Concepts, the envelope, missions, pairing, sovereignty.'],
  ['reference', 'Reference', 'CLI and configuration.'],
  ['specification', 'Specification', 'Handlers, transitions, worked examples.'],
  ['integrations', 'Integrations', 'Editors, model providers, tools.'],
  ['use-cases', 'Use cases', 'Worked recipes.'],
];

export const GET: APIRoute = async () => {
  const docs = (await getCollection('docs', published)).sort((a, b) => a.id.localeCompare(b.id));
  const pick = (prefix: string) => docs.filter((e) => e.id === prefix || e.id.startsWith(`${prefix}/`));

  const line = (e: (typeof docs)[number]) =>
    `- [${entryTitle(e)}](${SITE}/docs/${e.id}/)${e.data.description ? `: ${e.data.description}` : ''}`;

  const out: string[] = [
    '# contenox',
    '',
    '> Guardrails for AI agents. Most guardrails check what the model said; contenox gates what the agent does. One file in your repository declares what may run, what needs a human, and what starts work, and the task engine enforces it before every tool call. Open source under Apache-2.0, one binary, local SQLite, no account. Runs on your machine, against your files, with your keys, on the AI model you picked.',
    '',
    'Terminology, so a summary does not invent one: the declaration file is the **envelope**; approvals gated by it are **human-in-the-loop**; a unit of unattended work is a **mission**; the surfaces (terminal UI, CLI, editors over ACP, browser via the optional relay) are example implementations, not the product.',
    '',
    'What contenox is *not*: not a chat product, not a dashboard, not a hosted agent, not a compliance certification, and not autonomous — it does what was declared and stops where the declaration says stop.',
    '',
    '## Start here',
    '',
    `- [What contenox is](${SITE}/docs/guide/what-contenox-is/): the product in one page — what it is, why it exists, what it is typically used for, and what it is not.`,
    `- [Quickstart](${SITE}/docs/guide/quickstart/): install and first mission.`,
    `- [Core concepts](${SITE}/docs/guide/concepts/): chains, tasks, tools, transitions.`,
    '',
    '## Machine-readable',
    '',
    `- [HITL policy JSON Schema](${SITE}/schema/hitl-policy-v1.schema.json): the envelope format, generated from the Go types that load it.`,
    `- [Task chain JSON Schema](${SITE}/schema/task-chain.schema.json): the chain file format, same source.`,
    `- [Full text](${SITE}/llms-full.txt): every documentation page concatenated.`,
    `- [Sitemap](${SITE}/sitemap-index.xml): the complete URL list.`,
    '',
  ];

  for (const [prefix, heading, blurb] of SECTIONS) {
    const entries = pick(prefix);
    if (!entries.length) continue;
    out.push(`## ${heading}`, '', blurb, '', ...entries.map(line), '');
  }

  out.push(
    '## Legal',
    '',
    `- [Legal index](${SITE}/legal): terms, privacy, withdrawal, imprint, security, sub-processors for the optional hosted service.`,
    '',
    '## Source',
    '',
    '- [github.com/contenox/contenox](https://github.com/contenox/contenox): Apache-2.0.',
    '',
  );

  return new Response(out.join('\n'), {
    headers: { 'content-type': 'text/plain; charset=utf-8' },
  });
};
