import { describe, expect, it, vi } from 'vitest';

import { parseSlashInvocation, SlashCommandRegistry } from './registry';
import type { SlashCommand } from './types';

describe('parseSlashInvocation', () => {
  it('returns null for non-trigger text', () => {
    expect(parseSlashInvocation('hello')).toBeNull();
    expect(parseSlashInvocation(' /file foo')).toBeNull();
    expect(parseSlashInvocation(' @file foo')).toBeNull();
  });

  it('parses bare slash command', () => {
    expect(parseSlashInvocation('/help')).toEqual({
      trigger: '/',
      name: 'help',
      rawArgs: '',
      body: '',
    });
  });

  it('parses bare @-mention', () => {
    expect(parseSlashInvocation('@plan')).toEqual({
      trigger: '@',
      name: 'plan',
      rawArgs: '',
      body: '',
    });
  });

  it('parses @-mention with args', () => {
    expect(parseSlashInvocation('@file foo.go')).toEqual({
      trigger: '@',
      name: 'file',
      rawArgs: 'foo.go',
      body: '',
    });
  });

  it('separates body from invocation on first newline', () => {
    expect(parseSlashInvocation('@file foo.go\nexplain this')).toEqual({
      trigger: '@',
      name: 'file',
      rawArgs: 'foo.go',
      body: 'explain this',
    });
  });

  it('lowercases name; preserves args casing', () => {
    expect(parseSlashInvocation('@FILE Foo.GO')).toEqual({
      trigger: '@',
      name: 'file',
      rawArgs: 'Foo.GO',
      body: '',
    });
  });

  it('rejects digit-leading names', () => {
    expect(parseSlashInvocation('/123 hello')).toBeNull();
    expect(parseSlashInvocation('@123 hello')).toBeNull();
  });

  it('strips leading blank lines from body', () => {
    expect(parseSlashInvocation('/help\n\n\nactual body')).toEqual({
      trigger: '/',
      name: 'help',
      rawArgs: '',
      body: 'actual body',
    });
  });
});

function cmd(
  trigger: '/' | '@',
  name: string,
  aliases?: string[],
): SlashCommand {
  return {
    trigger,
    name,
    aliases,
    description: name,
    execute: () => undefined,
  };
}

describe('SlashCommandRegistry', () => {
  it('looks up by trigger + name (case-insensitive)', () => {
    const reg = new SlashCommandRegistry();
    reg.register(cmd('@', 'terminal', ['term']));
    expect(reg.get('@', 'TERMINAL')?.name).toBe('terminal');
    expect(reg.get('@', 'term')?.name).toBe('terminal');
    expect(reg.get('@', 'nope')).toBeUndefined();
    // Same name on the other trigger does NOT collide.
    expect(reg.get('/', 'terminal')).toBeUndefined();
  });

  it('separates trigger namespaces', () => {
    const reg = new SlashCommandRegistry();
    const slashFile = cmd('/', 'file');
    const atFile = cmd('@', 'file');
    reg.register(slashFile);
    reg.register(atFile);
    expect(reg.get('/', 'file')).toBe(slashFile);
    expect(reg.get('@', 'file')).toBe(atFile);
  });

  it('list groups slash entries before @ entries', () => {
    const reg = new SlashCommandRegistry();
    reg.register(cmd('@', 'file'));
    reg.register(cmd('/', 'help'));
    reg.register(cmd('@', 'plan'));
    expect(reg.list().map((c) => `${c.trigger}${c.name}`)).toEqual([
      '/help',
      '@file',
      '@plan',
    ]);
  });

  it('namesStartingWith filters by trigger', () => {
    const reg = new SlashCommandRegistry();
    reg.register(cmd('@', 'file'));
    reg.register(cmd('@', 'plan'));
    reg.register(cmd('/', 'help'));
    expect(reg.namesStartingWith('@', 'p')).toEqual(['plan']);
    expect(reg.namesStartingWith('/', 'h')).toEqual(['help']);
  });

  it('unregister drops both name and aliases under that trigger', () => {
    const reg = new SlashCommandRegistry();
    const spy = vi.fn();
    reg.subscribe(spy);
    const un = reg.register(cmd('@', 'terminal', ['term']));
    expect(spy).toHaveBeenCalledTimes(1);
    un();
    expect(reg.get('@', 'terminal')).toBeUndefined();
    expect(reg.get('@', 'term')).toBeUndefined();
    expect(spy).toHaveBeenCalledTimes(2);
  });

  it('registering same trigger+name twice replaces the previous entry', () => {
    const reg = new SlashCommandRegistry();
    const a = cmd('@', 'file');
    const b = cmd('@', 'file');
    reg.register(a);
    reg.register(b);
    expect(reg.get('@', 'file')).toBe(b);
  });
});
