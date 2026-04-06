import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import path from 'path';
import type { Plugin } from 'vite';
import { defineConfig, loadEnv } from 'vite';

/**
 * When using the dev API proxy, the browser only talks to the Vite origin.
 * Without a fallback, direct navigation to e.g. /chats 404s because there is no static file.
 * Mirrors the Go embed handler’s SPA fallback for non-/api routes.
 */
function beamSpaFallback(): Plugin {
  return {
    name: 'beam-spa-fallback',
    enforce: 'pre',
    configureServer(server) {
      server.middlewares.use((req, _res, next) => {
        if (req.method !== 'GET' || !req.url) {
          next();
          return;
        }
        const pathname = req.url.split('?')[0] ?? '';
        if (pathname.startsWith('/api')) {
          next();
          return;
        }
        if (
          pathname.startsWith('/@') ||
          pathname.startsWith('/node_modules') ||
          pathname.startsWith('/src')
        ) {
          next();
          return;
        }
        if (/\.[a-zA-Z0-9]+$/.test(pathname)) {
          next();
          return;
        }
        if (pathname === '/' || pathname === '') {
          next();
          return;
        }
        const q = req.url.includes('?') ? `?${req.url.split('?')[1]}` : '';
        req.url = `/index.html${q}`;
        next();
      });
    },
  };
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const devApiProxy =
    env.VITE_DEV_API_PROXY === '1' || env.VITE_DEV_API_PROXY === 'true';
  const proxyTarget = env.VITE_DEV_PROXY_TARGET || 'http://127.0.0.1:8081';

  return {
    plugins: [
      react(),
      tailwindcss(),
      ...(devApiProxy ? [beamSpaFallback()] : []),
    ],
    resolve: {
      alias: {
        '@': path.resolve(__dirname, './src'),
      },
    },
    build: {
      outDir: '../../internal/web/beam/dist',
      emptyOutDir: true,
    },
    /** Root-relative URLs so deep links (e.g. /chat/:id) still load /assets/* from the server root. */
    base: '/',
    server: devApiProxy
      ? {
          proxy: {
            '/api': {
              target: proxyTarget,
              changeOrigin: true,
            },
          },
        }
      : undefined,
  };
});
