import React, { createContext, ReactNode, useContext, useEffect, useState, useSyncExternalStore } from 'react';

interface ThemeContextType {
  theme: string;
  toggleTheme: () => void;
}

const ThemeContext = createContext<ThemeContextType | undefined>(undefined);

interface ThemeProviderProps {
  children: ReactNode;
}

/** Subscribe to OS prefers-color-scheme changes via matchMedia. */
const darkMQ = typeof window !== 'undefined' ? window.matchMedia('(prefers-color-scheme: dark)') : null;

function subscribeToScheme(cb: () => void) {
  darkMQ?.addEventListener('change', cb);
  return () => darkMQ?.removeEventListener('change', cb);
}

function getSystemTheme(): string {
  return darkMQ?.matches ? 'dark' : 'light';
}

export const ThemeProvider: React.FC<ThemeProviderProps> = ({ children }) => {
  const systemTheme = useSyncExternalStore(subscribeToScheme, getSystemTheme, () => 'light');
  const [theme, setTheme] = useState(systemTheme);

  // Sync with OS preference changes
  useEffect(() => {
    setTheme(systemTheme);
  }, [systemTheme]);

  const toggleTheme = () => {
    setTheme(prev => (prev === 'light' ? 'dark' : 'light'));
  };

  useEffect(() => {
    document.documentElement.className = theme;
  }, [theme]);

  return <ThemeContext.Provider value={{ theme, toggleTheme }}>{children}</ThemeContext.Provider>;
};

export const useTheme = (): ThemeContextType => {
  const context = useContext(ThemeContext);
  if (!context) {
    throw new Error('useTheme must be used within a ThemeProvider');
  }
  return context;
};
