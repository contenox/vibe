import type { ChatContextArtifact } from '../types';

/**
 * Context passed to every slash command's `execute`. Keeps the command itself
 * decoupled from React state — commands call into this surface, which then
 * arms artifacts via the [ArtifactRegistry] or produces inline feedback.
 */
export interface SlashCommandContext {
  /**
   * Arm a one-shot artifact source. It will attach on the very next turn,
   * then auto-unregister so subsequent turns are clean.
   *
   * `sourceId` must be stable per command so the same invocation overwrites
   * its own prior arming (e.g. `/file a.ts` then `/file b.ts` in quick
   * succession keeps only the latter).
   */
  armArtifact: (sourceId: string, label: string, artifact: ChatContextArtifact) => void;

  /**
   * Show a transient, non-blocking message in the composer footer (success,
   * help text, friendly error). Clears on next input or next command.
   */
  notify: (level: 'info' | 'error', message: string) => void;

  /** The command name the user typed (without the leading slash). */
  readonly commandName: string;
  /** The raw argument tail, trimmed. Empty string when no args. */
  readonly rawArgs: string;
}

/**
 * Trigger character that opens this entry in the composer.
 *
 *   '/' — actions (run, do something, mutate state). Examples: /help, /clear.
 *   '@' — context mentions (pull a thing INTO the agent's context). Examples:
 *         @file <path>, @plan, @terminal.
 *
 * The split mirrors industry convention (Cursor, Continue, Claude Code): the
 * `@` namespace is reserved for "make the agent see X"; the `/` namespace is
 * reserved for actions. Mixing them ("/file" to attach a file) is a footgun
 * the user gave specific feedback about.
 */
export type CommandTrigger = '/' | '@';

/**
 * A slash- or @-mention entry registered in [SlashCommandRegistry]. The class
 * name is historical (it predates the @ split); think of it as
 * "ChatCommand" — entries are dispatched by trigger + name. Built-ins live
 * in `builtins.ts`; UI surfaces (TerminalPanel, WorkspaceSplitPanel)
 * contribute their own via [useSlashCommand].
 */
export interface SlashCommand {
  /**
   * Trigger character. Defaults to '/' when omitted (legacy). New entries
   * should set this explicitly.
   */
  trigger?: CommandTrigger;
  /** Primary name, e.g. `file`. Typed as `<trigger><name> …`. */
  name: string;
  /** Optional additional names that dispatch to this same entry. */
  aliases?: string[];
  /** One-line description shown by `/help`. */
  description: string;
  /** Usage hint shown by `/help`, e.g. `@file <path>` or `/help`. */
  usage?: string;
  /**
   * Execute the entry. May be async (e.g. to fetch a file). Errors thrown
   * here are surfaced via `ctx.notify('error', ...)` by the host, so the
   * implementation can just `throw new Error('nope')`.
   */
  execute: (ctx: SlashCommandContext) => void | Promise<void>;
}
