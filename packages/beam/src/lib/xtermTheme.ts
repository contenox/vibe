import { useEffect, useState } from 'react';
import type { ITheme } from '@xterm/xterm';

function isDark(): boolean {
  if (typeof document === 'undefined') return false;
  const root = document.documentElement;
  // Check explicit class first (user toggle), fall back to OS preference
  if (root.classList.contains('dark')) return true;
  if (root.classList.contains('light')) return false;
  return window.matchMedia('(prefers-color-scheme: dark)').matches;
}

// Colors pulled from ui/src/index.css design tokens.
const LIGHT_THEME: ITheme = {
  background: '#f8f9fa', // surface-50
  foreground: '#2b2f33', // text
  cursor: '#2b2f33',
  cursorAccent: '#f8f9fa',
  selectionBackground: '#3b82f633', // primary-500 / 20%
  selectionForeground: undefined,
  black: '#212529', // surface-900
  red: '#dc2626', // error-500
  green: '#16a34a', // success-600
  yellow: '#ca8a04', // warning-600
  blue: '#2563eb', // primary-600
  magenta: '#9333ea',
  cyan: '#0891b2',
  white: '#f8f9fa', // surface-50
  brightBlack: '#6c757d', // surface-600
  brightRed: '#ef4444', // error-400 (light)
  brightGreen: '#22c55e', // success-500
  brightYellow: '#eab308', // warning-500
  brightBlue: '#3b82f6', // primary-500
  brightMagenta: '#a855f7',
  brightCyan: '#06b6d4',
  brightWhite: '#ffffff',
};

const DARK_THEME: ITheme = {
  background: '#0d1117', // dark-surface-50
  foreground: '#e2e5e9', // dark-text
  cursor: '#e2e5e9',
  cursorAccent: '#0d1117',
  selectionBackground: '#3b82f633',
  selectionForeground: undefined,
  black: '#0d1117', // dark-surface-50
  red: '#f87171', // dark-error-800
  green: '#4ade80', // dark-success-600
  yellow: '#facc15', // dark-warning-600
  blue: '#60a5fa', // dark-primary-300 approx
  magenta: '#c084fc',
  cyan: '#22d3ee',
  white: '#e2e5e9', // dark-text
  brightBlack: '#495880', // dark-surface-900
  brightRed: '#fca5a5', // dark-error-900
  brightGreen: '#86efac', // dark-success-700
  brightYellow: '#fde047', // dark-warning-700
  brightBlue: '#93c5fd',
  brightMagenta: '#d8b4fe',
  brightCyan: '#67e8f9',
  brightWhite: '#ffffff',
};

export function useXtermTheme(): ITheme {
  const [theme, setTheme] = useState<ITheme>(() => (isDark() ? DARK_THEME : LIGHT_THEME));

  useEffect(() => {
    const sync = () => setTheme(isDark() ? DARK_THEME : LIGHT_THEME);

    // Watch for class="dark" toggle on <html>
    const el = document.documentElement;
    const mo = new MutationObserver(sync);
    mo.observe(el, { attributes: true, attributeFilter: ['class'] });

    // Watch for OS preference change
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    mq.addEventListener('change', sync);

    return () => {
      mo.disconnect();
      mq.removeEventListener('change', sync);
    };
  }, []);

  return theme;
}
