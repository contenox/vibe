import { describe, expect, it } from 'vitest';

import { Artifact } from '../artifacts/types';
import { artifactToInlineAttachment, artifactsToInlineAttachments } from './index';

describe('artifactToInlineAttachment', () => {
  it('maps file_excerpt → file_view', () => {
    const a = Artifact.fileExcerpt({ path: 'a.ts', text: 'x', truncated: true });
    expect(artifactToInlineAttachment(a)).toEqual({
      kind: 'file_view',
      path: 'a.ts',
      text: 'x',
      truncated: true,
    });
  });

  it('maps open_file → file_view', () => {
    const a = Artifact.openFile({ path: 'b.ts', text: 'y' });
    expect(artifactToInlineAttachment(a)).toEqual({
      kind: 'file_view',
      path: 'b.ts',
      text: 'y',
    });
  });

  it('maps terminal_output → terminal_excerpt', () => {
    const a = Artifact.terminalOutput({ command: 'ls', output: 'a\nb\n' });
    expect(artifactToInlineAttachment(a)).toMatchObject({
      kind: 'terminal_excerpt',
      command: 'ls',
      output: 'a\nb\n',
    });
  });

  it('maps command_output → terminal_excerpt', () => {
    const a = Artifact.commandOutput({ command: 'go test', output: 'ok\n' });
    expect(artifactToInlineAttachment(a)).toMatchObject({
      kind: 'terminal_excerpt',
      command: 'go test',
      output: 'ok\n',
    });
  });

  it('maps plan_step → plan_summary', () => {
    const a = Artifact.planStep({
      plan_id: 'p1',
      ordinal: 3,
      description: 'd',
      status: 'failed',
      failure_class: 'capacity',
    });
    expect(artifactToInlineAttachment(a)).toEqual({
      kind: 'plan_summary',
      planId: 'p1',
      ordinal: 3,
      description: 'd',
      status: 'failed',
      summary: undefined,
      failureClass: 'capacity',
    });
  });

  it('maps runtime_state → state_unit', () => {
    const a = Artifact.runtimeState({ name: 'n', data: { a: 1 } });
    expect(artifactToInlineAttachment(a)).toEqual({
      kind: 'state_unit',
      name: 'n',
      data: { a: 1 },
    });
  });

  it('returns null for unknown / non-typed kinds', () => {
    expect(artifactToInlineAttachment({ kind: 'totally_custom' })).toBeNull();
  });
});

describe('artifactsToInlineAttachments', () => {
  it('drops nulls and preserves order', () => {
    const got = artifactsToInlineAttachments([
      Artifact.fileExcerpt({ path: 'a', text: '1' }),
      { kind: 'unknown' },
      Artifact.terminalOutput({ output: 'x' }),
    ]);
    expect(got.map((a) => a.kind)).toEqual(['file_view', 'terminal_excerpt']);
  });
});
