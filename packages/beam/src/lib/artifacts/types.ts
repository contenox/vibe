/**
 * Typed artifact vocabulary that mirrors chatsessionmodes/artifact_kinds.go.
 *
 * Every kind here has a Go-side renderer that turns its payload into a system
 * message before the LLM call. Adding a new kind is a two-sided change:
 * (a) add it here; (b) add a renderer + test in chatsessionmodes/artifact_kinds.go.
 *
 * The server still accepts any regex-valid kind string, so experimental kinds
 * can be sent before they land in this file — they just fall back to the
 * flat `{json}` rendering instead of a typed system message.
 */

import type { ChatContextArtifact } from '../types';

export const ARTIFACT_KINDS = [
  'file_excerpt',
  'terminal_output',
  'open_file',
  'plan_step',
  'command_output',
  'runtime_state',
] as const;

export type ArtifactKind = (typeof ARTIFACT_KINDS)[number];

export interface FileExcerptPayload {
  path: string;
  text: string;
  truncated?: boolean;
}

export interface TerminalOutputPayload {
  session_id?: string;
  command?: string;
  output: string;
  captured_at?: string;
  truncated?: boolean;
}

export interface OpenFilePayload {
  path: string;
  text: string;
  line_from?: number;
  line_to?: number;
}

export interface PlanStepPayload {
  plan_id: string;
  ordinal: number;
  description: string;
  status: string;
  summary?: string;
  failure_class?: string;
  last_failure?: string;
}

export interface CommandOutputPayload {
  command: string;
  output: string;
  exit_code?: number;
}

export interface RuntimeStatePayload {
  name: string;
  data?: unknown;
}

/**
 * Discriminated union over first-party kinds. Use this when you want the
 * compiler to narrow payload shape based on kind. Third-party kinds still flow
 * through the raw [ChatContextArtifact] type.
 */
export type TypedArtifact =
  | { kind: 'file_excerpt'; payload: FileExcerptPayload }
  | { kind: 'terminal_output'; payload: TerminalOutputPayload }
  | { kind: 'open_file'; payload: OpenFilePayload }
  | { kind: 'plan_step'; payload: PlanStepPayload }
  | { kind: 'command_output'; payload: CommandOutputPayload }
  | { kind: 'runtime_state'; payload: RuntimeStatePayload };

/** Narrow [ChatContextArtifact] to [TypedArtifact] when kind is first-party. */
export function isTypedArtifact(a: ChatContextArtifact): a is TypedArtifact {
  return (ARTIFACT_KINDS as readonly string[]).includes(a.kind);
}

/** Type-safe constructors for each first-party kind. */
export const Artifact = {
  fileExcerpt(p: FileExcerptPayload): TypedArtifact {
    return { kind: 'file_excerpt', payload: p };
  },
  terminalOutput(p: TerminalOutputPayload): TypedArtifact {
    return { kind: 'terminal_output', payload: p };
  },
  openFile(p: OpenFilePayload): TypedArtifact {
    return { kind: 'open_file', payload: p };
  },
  planStep(p: PlanStepPayload): TypedArtifact {
    return { kind: 'plan_step', payload: p };
  },
  commandOutput(p: CommandOutputPayload): TypedArtifact {
    return { kind: 'command_output', payload: p };
  },
  runtimeState(p: RuntimeStatePayload): TypedArtifact {
    return { kind: 'runtime_state', payload: p };
  },
};
