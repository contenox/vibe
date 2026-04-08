import { useMemo } from 'react';
import type { ITheme } from '@xterm/xterm';
import { useTheme } from './ThemeProvider';

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
  background: '#0d0e10', // dark-surface-100
  foreground: '#e8eaed', // dark-text
  cursor: '#e8eaed',
  cursorAccent: '#0d0e10',
  selectionBackground: '#3b82f633',
  selectionForeground: undefined,
  black: '#08090a', // dark-surface-50
  red: '#f87171', // dark-error-800
  green: '#4ade80', // dark-success-600
  yellow: '#facc15', // dark-warning-600
  blue: '#60a5fa', // dark-primary-300 approx
  magenta: '#c084fc',
  cyan: '#22d3ee',
  white: '#e8eaed', // dark-text
  brightBlack: '#4b4f56', // dark-surface-700
  brightRed: '#fca5a5', // dark-error-900
  brightGreen: '#86efac', // dark-success-700
  brightYellow: '#fde047', // dark-warning-700
  brightBlue: '#93c5fd',
  brightMagenta: '#d8b4fe',
  brightCyan: '#67e8f9',
  brightWhite: '#ffffff',
};

/** Matches Beam [ThemeProvider] (`html.dark` / `html.light`) so xterm matches Monaco and the shell. */
export function useXtermTheme(): ITheme {
  const { theme } = useTheme();
  return useMemo(() => (theme === 'dark' ? DARK_THEME : LIGHT_THEME), [theme]);
}
