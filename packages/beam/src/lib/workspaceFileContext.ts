import { apiFetchBinary } from './fetch';
import { api } from './api';
import type { ChatContextArtifact, ChatContextPayload } from './types';

/** Matches chatsessionmodes/inject_artifacts.go maxArtifactPayloadBytes */
export const MAX_ARTIFACT_PAYLOAD_UTF8_BYTES = 32768;
/** Max file size to load into the workspace editor (bytes). */
export const MAX_EDITOR_FILE_BYTES = 4 * 1024 * 1024;

const te = new TextEncoder();

function utf8ByteLength(s: string): number {
  return te.encode(s).length;
}

export function bufferLooksBinary(buf: ArrayBuffer): boolean {
  const u = new Uint8Array(buf);
  const n = Math.min(u.length, 8192);
  for (let i = 0; i < n; i++) {
    if (u[i] === 0) return true;
  }
  return false;
}

/**
 * Download VFS file as UTF-8 text. Throws if too large or likely binary.
 */
export async function readWorkspaceFileText(
  fileId: string,
  signal?: AbortSignal,
): Promise<string> {
  const url = api.getDownloadFileUrl(fileId) + (fileId.includes('?') ? '&' : '?') + 'skip=true';
  const buf = await apiFetchBinary(url, { signal });
  if (buf.byteLength > MAX_EDITOR_FILE_BYTES) {
    throw new Error('FILE_TOO_LARGE');
  }
  if (bufferLooksBinary(buf)) {
    throw new Error('FILE_BINARY');
  }
  return new TextDecoder('utf-8', { fatal: false }).decode(buf);
}

/**
 * Binary-search the largest prefix of `text` whose JSON payload (with the
 * supplied fixed keys) still fits under [MAX_ARTIFACT_PAYLOAD_UTF8_BYTES].
 * Shared across both file_excerpt and open_file builders so they agree on the
 * byte budget.
 */
function truncateTextToPayloadLimit(path: string, text: string): { text: string; truncated: boolean } {
  let lo = 0;
  let hi = text.length;
  let best = '';
  while (lo <= hi) {
    const mid = Math.floor((lo + hi) / 2);
    const slice = text.slice(0, mid);
    const payload = JSON.stringify({ path, text: slice });
    if (utf8ByteLength(payload) <= MAX_ARTIFACT_PAYLOAD_UTF8_BYTES) {
      best = slice;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return { text: best, truncated: best.length < text.length };
}

/**
 * Build a single file_excerpt artifact; truncates `text` so JSON payload fits server limits.
 */
export function buildFileExcerptArtifact(path: string, text: string): ChatContextArtifact {
  const { text: fitted } = truncateTextToPayloadLimit(path, text);
  return {
    kind: 'file_excerpt',
    payload: { path, text: fitted },
  };
}

/**
 * Build an open_file artifact — used when the UI wants the LLM to know this is
 * the file the user currently has open in the workspace (semantic: "this is
 * live") as opposed to a file_excerpt (semantic: "a snapshot I pasted").
 */
export function buildOpenFileArtifact(path: string, text: string): ChatContextArtifact {
  const { text: fitted } = truncateTextToPayloadLimit(path, text);
  return {
    kind: 'open_file',
    payload: { path, text: fitted },
  };
}

// buildWorkspaceChatContext was the legacy file_excerpt wrapper; superseded by
// the sticky open_file ArtifactSource registered in WorkspaceSplitPanel.
// Intentionally removed — add it back only if an external consumer surfaces.
