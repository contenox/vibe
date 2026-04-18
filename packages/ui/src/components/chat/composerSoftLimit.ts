/** Default soft reference for character count / warnings (128 KiB). */
export const DEFAULT_COMPOSER_SOFT_MAX = 131072;

/** True when the count should show the "approaching limit" warning band (same ratio as before). */
export function isComposerCharCountWarning(length: number, softMax: number): boolean {
  return length > softMax * 0.875;
}

/** True when optional soft-limit note should appear (over the guide). */
export function isOverComposerSoftMax(length: number, softMax: number): boolean {
  return length > softMax;
}
