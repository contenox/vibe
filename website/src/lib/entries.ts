import type { CollectionEntry } from 'astro:content';

type AnyEntry = CollectionEntry<'docs' | 'legal' | 'de'>;

// Title precedence: frontmatter, first markdown heading, slug.
export function entryTitle(entry: AnyEntry): string {
  if (entry.data.title) return entry.data.title;
  const m = entry.body?.match(/^#\s+(.+)$/m);
  if (m) return m[1].trim();
  return entry.id;
}

export function published(entry: AnyEntry): boolean {
  return entry.data.draft !== true;
}
