import { useEffect, useState } from 'react';

/**
 * True when Beam’s UI is using dark tokens: explicit `html.dark`, or OS dark mode
 * when there is no `html.light` override. Matches Tailwind v4 `@dark` / `dark:`
 * behavior without requiring a `dark` class on `<html>`.
 */
export function computeMonacoDarkTheme(): boolean {
  if (typeof document === 'undefined') return false;
  const root = document.documentElement;
  if (root.classList.contains('dark')) return true;
  if (root.classList.contains('light')) return false;
  return window.matchMedia('(prefers-color-scheme: dark)').matches;
}

export function useMonacoAppTheme(): 'vs-dark' | 'vs' {
  const [theme, setTheme] = useState<'vs-dark' | 'vs'>(() =>
    computeMonacoDarkTheme() ? 'vs-dark' : 'vs',
  );

  useEffect(() => {
    const el = document.documentElement;
    const sync = () => setTheme(computeMonacoDarkTheme() ? 'vs-dark' : 'vs');
    sync();

    const mo = new MutationObserver(sync);
    mo.observe(el, { attributes: true, attributeFilter: ['class'] });

    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    mq.addEventListener('change', sync);

    return () => {
      mo.disconnect();
      mq.removeEventListener('change', sync);
    };
  }, []);

  return theme;
}
