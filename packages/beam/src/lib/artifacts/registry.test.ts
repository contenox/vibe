import { describe, expect, it, vi } from 'vitest';

import { ArtifactRegistry, type ArtifactSource } from './registry';
import { Artifact } from './types';

function source(id: string, collect: ArtifactSource['collect']): ArtifactSource {
  return { id, label: id, collect };
}

describe('ArtifactRegistry', () => {
  it('collects artifacts in registration order and skips null sources', () => {
    const reg = new ArtifactRegistry();
    reg.register(
      source('files', () => Artifact.openFile({ path: 'a.ts', text: 'x' })),
    );
    reg.register(source('terminal', () => null));
    reg.register(
      source('cmd', () => Artifact.commandOutput({ command: 'ls', output: 'a\n' })),
    );

    const got = reg.collect();
    expect(got).toHaveLength(2);
    expect(got[0].kind).toBe('open_file');
    expect(got[1].kind).toBe('command_output');
  });

  it('unregister callback removes the source and notifies subscribers', () => {
    const reg = new ArtifactRegistry();
    const spy = vi.fn();
    reg.subscribe(spy);

    const unregister = reg.register(source('s', () => null));
    expect(reg.list()).toHaveLength(1);
    expect(spy).toHaveBeenCalledTimes(1);

    unregister();
    expect(reg.list()).toHaveLength(0);
    expect(spy).toHaveBeenCalledTimes(2);
  });

  it('replacing a source by id does not duplicate it', () => {
    const reg = new ArtifactRegistry();
    reg.register(source('s', () => Artifact.commandOutput({ command: 'one', output: '' })));
    reg.register(source('s', () => Artifact.commandOutput({ command: 'two', output: '' })));
    const got = reg.collect();
    expect(got).toHaveLength(1);
    expect((got[0].payload as { command: string }).command).toBe('two');
  });

  it('stale unregister calls do not remove a replacement source', () => {
    const reg = new ArtifactRegistry();
    const first = source('s', () => null);
    const unregister = reg.register(first);
    const second = source('s', () => Artifact.commandOutput({ command: 'replaced', output: '' }));
    reg.register(second);
    // The first source's unregister function should no-op now.
    unregister();
    expect(reg.list()).toHaveLength(1);
    expect(reg.list()[0]).toBe(second);
  });
});
