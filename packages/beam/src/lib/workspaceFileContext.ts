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
 * Build a single file_excerpt artifact; truncates `text` so JSON payload fits server limits.
 */
export function buildFileExcerptArtifact(path: string, text: string): ChatContextArtifact {
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
  return {
    kind: 'file_excerpt',
    payload: { path, text: best },
  };
}

/**
 * Wraps file_excerpt in ChatContextPayload for sendMessage; returns undefined if nothing to attach.
 */
export function buildWorkspaceChatContext(
  vfsPath: string | null,
  editorText: string,
  canAttach: boolean,
): ChatContextPayload | undefined {
  if (!canAttach || !vfsPath?.trim()) return undefined;
  const artifact = buildFileExcerptArtifact(vfsPath.trim(), editorText);
  const payloadStr = JSON.stringify(artifact.payload ?? {});
  if (utf8ByteLength(payloadStr) > MAX_ARTIFACT_PAYLOAD_UTF8_BYTES) {
    return undefined;
  }
  return { artifacts: [artifact] };
}
