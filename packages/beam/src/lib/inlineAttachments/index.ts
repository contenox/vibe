/**
 * Mapping helpers from artifact kinds (LLM-context shapes) to inline
 * attachment kinds (presentation shapes).
 *
 * The two vocabularies are intentionally distinct: an artifact is what gets
 * injected into the LLM as a system message; an attachment is what gets
 * rendered inline in the chat thread for the human to see. Some artifacts
 * never have a presentation form (e.g. terse runtime_state); some attachments
 * never need to flow into context (e.g. an inline diff with no relevant
 * payload).
 */

import type { ChatContextArtifact } from '../types';
import type { InlineAttachment } from '../types';
import {
  isTypedArtifact,
  type FileExcerptPayload,
  type OpenFilePayload,
  type TerminalOutputPayload,
  type PlanStepPayload,
  type CommandOutputPayload,
  type RuntimeStatePayload,
} from '../artifacts/types';

/**
 * Convert one artifact into an inline attachment for thread rendering.
 * Returns null when the artifact kind has no presentation form (caller
 * filters nulls).
 */
export function artifactToInlineAttachment(
  artifact: ChatContextArtifact,
): InlineAttachment | null {
  if (!isTypedArtifact(artifact)) return null;
  switch (artifact.kind) {
    case 'file_excerpt': {
      const p = artifact.payload as FileExcerptPayload;
      return { kind: 'file_view', path: p.path, text: p.text, truncated: p.truncated };
    }
    case 'open_file': {
      const p = artifact.payload as OpenFilePayload;
      return { kind: 'file_view', path: p.path, text: p.text };
    }
    case 'terminal_output': {
      const p = artifact.payload as TerminalOutputPayload;
      return {
        kind: 'terminal_excerpt',
        output: p.output,
        command: p.command,
        sessionId: p.session_id,
        capturedAt: p.captured_at,
      };
    }
    case 'command_output': {
      const p = artifact.payload as CommandOutputPayload;
      return {
        kind: 'terminal_excerpt',
        output: p.output,
        command: p.command,
      };
    }
    case 'plan_step': {
      const p = artifact.payload as PlanStepPayload;
      return {
        kind: 'plan_summary',
        planId: p.plan_id,
        ordinal: p.ordinal,
        description: p.description,
        status: p.status,
        summary: p.summary,
        failureClass: p.failure_class,
      };
    }
    case 'runtime_state': {
      const p = artifact.payload as RuntimeStatePayload;
      return { kind: 'state_unit', name: p.name, data: p.data };
    }
    default:
      return null;
  }
}

/** Map a list of artifacts, dropping any with no inline form. */
export function artifactsToInlineAttachments(
  artifacts: ChatContextArtifact[],
): InlineAttachment[] {
  const out: InlineAttachment[] = [];
  for (const a of artifacts) {
    const inline = artifactToInlineAttachment(a);
    if (inline) out.push(inline);
  }
  return out;
}
