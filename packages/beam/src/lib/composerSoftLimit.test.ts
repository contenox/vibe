import { describe, expect, it } from 'vitest';

import {
  DEFAULT_COMPOSER_SOFT_MAX,
  isComposerCharCountWarning,
  isOverComposerSoftMax,
} from '@contenox/ui';

describe('composerSoftLimit (re-exported from @contenox/ui)', () => {
  it('warns when past 87.5% of soft max', () => {
    const soft = 1000;
    expect(isComposerCharCountWarning(874, soft)).toBe(false);
    expect(isComposerCharCountWarning(876, soft)).toBe(true);
  });

  it('overSoftMax when strictly above soft max', () => {
    expect(isOverComposerSoftMax(100, 100)).toBe(false);
    expect(isOverComposerSoftMax(101, 100)).toBe(true);
  });

  it('default constant is 128 KiB', () => {
    expect(DEFAULT_COMPOSER_SOFT_MAX).toBe(128 * 1024);
  });
});
