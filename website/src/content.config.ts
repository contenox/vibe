import { z, defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';

// All content is sourced from the runtime repo's docs/ tree — the website
// owns no content, and every page renders under /docs/. Frontmatter is
// optional; pages without a title fall back to their first heading.
const docsSchema = z.object({
  title: z.string().optional(),
  description: z.string().optional(),
  draft: z.boolean().optional(),
  /** Sidebar position within its folder. Unordered pages follow, alphabetically. */
  order: z.number().optional(),
});

const docs = defineCollection({
  // development/internal/** holds work-records, not published pages.
  // legal.md and de/** render outside /docs/, from their own collections.
  loader: glob({
    pattern: ['**/*.md', '!development/internal/**', '!legal.md', '!legal/**', '!de/**'],
    base: '../docs',
  }),
  schema: docsSchema,
});

// legal.md is the site's own notice at /legal; legal/*.md are the hosted
// service's documents at /legal/<name>. contenox.com is where they are
// canonically published — app.contenox.com serves a synced copy.
const legal = defineCollection({
  loader: glob({ pattern: ['legal.md', 'legal/*.md'], base: '../docs' }),
  schema: docsSchema.extend({ eyebrow: z.string().optional() }),
});

// German pages, each rendered at /de/<id>/ by pages/de/[...slug].astro.
const deSchema = docsSchema.extend({
  /** Small label above the h1. */
  eyebrow: z.string().optional(),
  ogType: z.string().optional(),
  /** Path of the English counterpart, which makes the hreflang pair. */
  en: z.string().optional(),
  noindex: z.boolean().optional(),
});

const de = defineCollection({
  loader: glob({ pattern: '**/*.md', base: '../docs/de' }),
  schema: deSchema,
});

export const collections = { docs, legal, de };
