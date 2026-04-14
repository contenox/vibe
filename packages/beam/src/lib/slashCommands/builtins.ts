import { buildOpenFileArtifact } from '../workspaceFileContext';
import { Artifact } from '../artifacts/types';
import { type SlashCommand, type SlashCommandContext } from './types';
import type { SlashCommandRegistry } from './registry';

/**
 * `/help` — lists every registered command with usage + description. Uses
 * `notify('info', ...)` so the composer's footer displays it until the user
 * starts typing again. Built lazily against the live registry so commands
 * added after mount are discoverable.
 */
export function createHelpCommand(registry: SlashCommandRegistry): SlashCommand {
  return {
    trigger: '/',
    name: 'help',
    description: 'List every command and @-mention available in this session.',
    usage: '/help',
    execute: (ctx: SlashCommandContext) => {
      const cmds = registry.list();
      if (cmds.length === 0) {
        ctx.notify('info', 'No commands registered.');
        return;
      }
      const lines = cmds.map((c) => {
        const trigger = c.trigger ?? '/';
        const usage = c.usage ?? `${trigger}${c.name}`;
        return `${usage} — ${c.description}`;
      });
      ctx.notify('info', `Available:\n${lines.join('\n')}`);
    },
  };
}

/**
 * Abstracts the "resolve a path to text" dependency so the command stays pure.
 * Passed in by ChatPage, backed by the VFS list + download API.
 */
export interface FileResolver {
  /**
   * Given a user-supplied path, return the path + text to attach as an
   * open_file artifact. Implementations should:
   *   - tolerate leading slash or relative form (strip/normalize);
   *   - match first by exact path, then by basename-suffix match;
   *   - throw with a friendly message when no match, multiple matches, or
   *     the file is binary / too large.
   */
  resolve(path: string, signal?: AbortSignal): Promise<{ path: string; text: string }>;
}

/**
 * `/file <path>` — arm a one-shot `open_file` artifact for the given path. The
 * artifact fires on the next send and then auto-clears. This differs from
 * WorkspaceSplitPanel's sticky `open_file` source: `/file` is explicit, ad-hoc,
 * and fires once per command.
 */
export function createFileCommand(resolver: FileResolver): SlashCommand {
  return {
    trigger: '@',
    name: 'file',
    description: 'Mention a file from the workspace as context for the next message.',
    usage: '@file <path>',
    execute: async (ctx: SlashCommandContext) => {
      const path = ctx.rawArgs.trim();
      if (!path) {
        ctx.notify('error', 'Usage: @file <path>');
        return;
      }
      let resolved: { path: string; text: string };
      try {
        resolved = await resolver.resolve(path);
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err);
        ctx.notify('error', `@file: ${msg}`);
        return;
      }
      const artifact = buildOpenFileArtifact(resolved.path, resolved.text);
      ctx.armArtifact(`mention:file:${resolved.path}`, `@${resolved.path}`, artifact);
      ctx.notify('info', `Attached ${resolved.path} (will send with your next message).`);
    },
  };
}

/**
 * `/plan` — attach the active plan as a series of `plan_step` artifacts, one
 * per step. Useful when the user wants the agent to reason about the full
 * plan without pasting it.
 *
 * Kept minimal in phase 3: attaches up to the first 20 steps to stay under
 * the cumulative byte cap. Future phases may add `/plan <ordinal>` for a
 * single step.
 */
export interface PlanProvider {
  /** Return the current active plan's steps, or null if none. */
  getActivePlanSteps(): { planId: string; steps: Array<{
    ordinal: number;
    description: string;
    status: string;
    summary?: string;
    failureClass?: string;
    lastFailure?: string;
  }> } | null;
}

export function createPlanCommand(provider: PlanProvider): SlashCommand {
  return {
    trigger: '@',
    name: 'plan',
    description: 'Mention active plan steps as context.',
    usage: '@plan',
    execute: (ctx: SlashCommandContext) => {
      const active = provider.getActivePlanSteps();
      if (!active || active.steps.length === 0) {
        ctx.notify('error', '@plan: no active plan.');
        return;
      }
      const steps = active.steps.slice(0, 20);
      steps.forEach((step, i) => {
        const artifact = Artifact.planStep({
          plan_id: active.planId,
          ordinal: step.ordinal,
          description: step.description,
          status: step.status,
          summary: step.summary,
          failure_class: step.failureClass,
          last_failure: step.lastFailure,
        });
        ctx.armArtifact(
          `mention:plan:${active.planId}:${step.ordinal}:${i}`,
          `@plan:${step.ordinal}`,
          artifact,
        );
      });
      ctx.notify(
        'info',
        `Attached ${steps.length} plan step${steps.length === 1 ? '' : 's'} (will send with your next message).`,
      );
    },
  };
}
