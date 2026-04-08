import { useMemo } from 'react';
import type { editor } from 'monaco-editor';
import { useTheme } from './ThemeProvider';

/** Registered with `monaco.editor.defineTheme` — use with [Editor] `theme` + `beforeMount`. */
export const BEAM_MONACO_THEME_LIGHT = 'beam-light';
export const BEAM_MONACO_THEME_DARK = 'beam-dark';

// Hex values align with `xtermTheme.ts` / `@contenox/ui` surfaces (not raw `vs` / `vs-dark`).
const beamLight: editor.IStandaloneThemeData = {
  base: 'vs',
  inherit: true,
  rules: [],
  colors: {
    'editor.background': '#f8f9fa',
    'editor.foreground': '#2b2f33',
    'editorLineNumber.foreground': '#6c757d',
    'editorLineNumber.activeForeground': '#2b2f33',
    'editorCursor.foreground': '#2b2f33',
    'editor.selectionBackground': '#3b82f633',
    'editor.inactiveSelectionBackground': '#3b82f626',
    'editor.lineHighlightBackground': '#e9ecef',
    'editorLineNumber.background': '#f8f9fa',
    'editorIndentGuide.background': '#dee2e6',
    'editorIndentGuide.activeBackground': '#adb5bd',
    'editorWhitespace.foreground': '#ced4da',
    'scrollbarSlider.background': '#adb5bd80',
    'scrollbarSlider.hoverBackground': '#6c757d99',
    'scrollbarSlider.activeBackground': '#495057b3',
    'editorWidget.background': '#ffffff',
    'editorWidget.border': '#dee2e6',
    'editorSuggestWidget.background': '#ffffff',
    'editorSuggestWidget.border': '#dee2e6',
    'peekViewTitle.background': '#f1f3f5',
    'peekViewEditor.background': '#f8f9fa',
    'minimap.background': '#f8f9fa',
  },
};

const beamDark: editor.IStandaloneThemeData = {
  base: 'vs-dark',
  inherit: true,
  rules: [],
  colors: {
    'editor.background': '#0d0e10', // dark-surface-100
    'editor.foreground': '#e8eaed', // dark-text
    'editorLineNumber.foreground': '#5f636b', // dark-surface-800
    'editorLineNumber.activeForeground': '#e8eaed',
    'editorCursor.foreground': '#e8eaed',
    'editor.selectionBackground': '#264f7866',
    'editor.inactiveSelectionBackground': '#264f7840',
    'editor.lineHighlightBackground': '#1a1c1f80', // dark-surface-300
    'editorLineNumber.background': '#0d0e10',
    'editorIndentGuide.background': '#24262a', // dark-surface-400
    'editorIndentGuide.activeBackground': '#4b4f56', // dark-surface-700
    'editorWhitespace.foreground': '#3b3e44', // dark-surface-600
    'scrollbarSlider.background': '#3b3e4480', // dark-surface-600
    'scrollbarSlider.hoverBackground': '#5f636b99', // dark-surface-800
    'scrollbarSlider.activeBackground': '#757980b3', // dark-surface-900
    'editorWidget.background': '#1a1c1f', // dark-surface-300
    'editorWidget.border': '#24262a', // dark-surface-400
    'editorSuggestWidget.background': '#1a1c1f',
    'editorSuggestWidget.border': '#24262a',
    'peekViewTitle.background': '#1a1c1f',
    'peekViewEditor.background': '#0d0e10',
    'minimap.background': '#0d0e10',
  },
};

type MonacoNS = typeof import('monaco-editor');

/**
 * Call once per app from [@monaco-editor/react] `beforeMount` so Monaco uses Beam surfaces
 * instead of stock `vs` / `vs-dark`.
 */
export function defineBeamMonacoThemes(monaco: MonacoNS): void {
  monaco.editor.defineTheme(BEAM_MONACO_THEME_LIGHT, beamLight);
  monaco.editor.defineTheme(BEAM_MONACO_THEME_DARK, beamDark);
}

/**
 * Theme id for [Editor] `theme` — driven by [ThemeProvider] (same as xterm).
 */
export function useMonacoAppTheme(): typeof BEAM_MONACO_THEME_LIGHT | typeof BEAM_MONACO_THEME_DARK {
  const { theme } = useTheme();
  return useMemo(
    () => (theme === 'dark' ? BEAM_MONACO_THEME_DARK : BEAM_MONACO_THEME_LIGHT),
    [theme],
  );
}
