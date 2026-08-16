import { getCollection } from 'astro:content';
import { entryTitle, published } from './entries';

export interface TreeNode {
  name: string;
  /** Route url when this node is a page. */
  url?: string;
  title?: string;
  order?: number;
  children: TreeNode[];
}

function insert(root: TreeNode, parts: string[], url: string, title: string, order?: number) {
  const lastName = parts[parts.length - 1];
  // The glob loader slugifies path segments, so README.md arrives as "readme".
  const isIndex = lastName === 'index' || lastName.toLowerCase() === 'readme';

  let node = root;
  for (const part of parts.slice(0, -1)) {
    let child = node.children.find((c) => c.name === part);
    if (!child) {
      child = { name: part, children: [] };
      node.children.push(child);
    }
    node = child;
  }

  if (isIndex) {
    // Promote the index URL + title to the folder node itself.
    node.url = url;
    if (!node.title) node.title = title;
    if (order !== undefined) node.order = order;
  } else {
    node.children.push({ name: lastName, url, title, order, children: [] });
  }
}

/** Section reading order. Both tails last: contributor docs, then the Lab. */
export const SECTION_ORDER = [
  'guide',
  'integrations',
  'use-cases',
  'reference',
  'specification',
  'development',
  'rnd',
];

const sectionRank = (name: string) => {
  const i = SECTION_ORDER.indexOf(name);
  return i === -1 ? SECTION_ORDER.length : i;
};

function sortTree(node: TreeNode, depth = 0) {
  // Index first, then pages that declare an order, then the rest alphabetically.
  node.children.sort((a, b) => {
    if (depth === 0) {
      const r = sectionRank(a.name) - sectionRank(b.name);
      if (r !== 0) return r;
      return a.name.localeCompare(b.name);
    }
    const rank = (n: TreeNode) => (n.name.toLowerCase() === 'readme' || n.name === 'index' ? 0 : 1);
    if (rank(a) !== rank(b)) return rank(a) - rank(b);
    const ord = (n: TreeNode) => (n.order ?? Number.MAX_SAFE_INTEGER);
    if (ord(a) !== ord(b)) return ord(a) - ord(b);
    return a.name.localeCompare(b.name);
  });
  node.children.forEach((c) => sortTree(c, depth + 1));
}

/** Builds the sidebar tree mirroring the docs/ folder structure exactly. */
export async function buildDocsTree(): Promise<TreeNode> {
  const root: TreeNode = { name: '', children: [] };
  for (const entry of await getCollection('docs', published)) {
    insert(root, entry.id.split('/'), `/docs/${entry.id}/`, entryTitle(entry), entry.data.order);
  }
  sortTree(root);
  return root;
}

/** Returns the top-level nav sections derived from the docs tree.
 *  Sections without their own landing page anchor into the /docs/ index. */
export async function buildNavSections(): Promise<{ name: string; url: string }[]> {
  const tree = await buildDocsTree();
  // Display overrides for sections whose directory name isn't the label.
  const labels: Record<string, string> = { rnd: 'Lab', specification: 'Formats' };
  return tree.children
    .filter((n) => n.url || n.children.length > 0)
    .map((n) => ({
      name: labels[n.name] ?? n.name,
      url: n.url ?? `/docs/#${n.name}`,
    }));
}
