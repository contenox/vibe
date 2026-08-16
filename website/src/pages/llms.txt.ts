import type { APIRoute } from 'astro';
import { getCollection } from 'astro:content';
import { entryTitle, published } from '../lib/entries';
import { PRODUCT_SHAPES, PRODUCT_SUMMARY } from '../lib/summary';

const SITE = 'https://contenox.com';

const SECTIONS: [string, string, string][] = [
  ['guide', 'Guides', 'Declaring agents, the envelope, missions and the durable ask, pairing, sovereignty.'],
  ['reference', 'Reference', 'The CLI, agents.toml, and configuration.'],
  ['specification', 'Specification', 'The compiled chain format: handlers, transitions, worked examples.'],
  ['integrations', 'Integrations', 'Editors over ACP, model providers, MCP servers and OpenAPI tools.'],
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
    `> ${PRODUCT_SUMMARY}`,
    '',
    PRODUCT_SHAPES,
    '',
    'Terminology, so a summary does not invent one: the policy file bounding a run is the **envelope**; approvals gated by it are **human-in-the-loop**; a paused run that outlives its own process is the **durable ask**; a unit of unattended work is a **mission**. The agents that ship in the box (`acp`, `acpx`, `triage`, `reviewer`, `researcher`) are examples of the artifact, the default page rather than the product, and the surfaces (CLI, editors over ACP, browser via the optional relay) are example implementations, not the product either.',
    '',
    '## Start here',
    '',
    `- [What contenox is](${SITE}/docs/guide/what-contenox-is/): the product in one page — what it is, why it exists, what it is typically used for, and what it is not.`,
    `- [Declaring agents](${SITE}/docs/guide/agents/): the artifact — the frontmatter, where declarations live, the directory that becomes a workflow.`,
    `- [The envelope](${SITE}/docs/guide/hitl/): what passes silently, what stops for a human, what is denied outright.`,
    `- [Quickstart](${SITE}/docs/guide/quickstart/): install and first mission.`,
    '',
    '## Machine-readable',
    '',
    `- [HITL policy JSON Schema](${SITE}/schema/hitl-policy-v1.schema.json): the envelope format, generated from the Go types that load it.`,
    `- [Task chain JSON Schema](${SITE}/schema/task-chain.schema.json): the compiled chain format, same source.`,
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
