import type { APIRoute } from 'astro';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

const SCHEMA_DIR = join(process.cwd(), '..', 'schema');

export function getStaticPaths() {
  return readdirSync(SCHEMA_DIR)
    .filter((f) => f.endsWith('.json'))
    .map((f) => ({ params: { file: f } }));
}

export const GET: APIRoute = ({ params }) => {
  const file = params.file!;
  if (!/^[a-z0-9.-]+\.json$/.test(file)) return new Response('not found', { status: 404 });
  return new Response(readFileSync(join(SCHEMA_DIR, file), 'utf-8'), {
    headers: { 'content-type': 'application/schema+json; charset=utf-8' },
  });
};
