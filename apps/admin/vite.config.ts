import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

// The admin UI is mounted under /manage (router basename + nginx location).
// `base` makes Vite emit asset URLs prefixed with /manage/ so the bundle
// resolves correctly when served behind that path.
export default defineConfig(({ mode }) => {
  // Load .env so the dev proxy can target a remote backend via
  // VITE_DEV_API_TARGET without pulling in @types/node for process.env.
  const env = loadEnv(mode, process.cwd(), 'VITE_');
  return {
    base: '/manage/',
    plugins: [react()],
    server: {
      port: 5174,
      proxy: {
        // Dev proxy to the manage-API (katalog-manager-api / chino-api).
        // Override the target with VITE_DEV_API_TARGET if your backend runs
        // somewhere other than localhost:8080.
        '/api': {
          target: env.VITE_DEV_API_TARGET || 'http://localhost:8080',
          changeOrigin: true,
        },
      },
    },
    build: {
      outDir: 'dist',
      sourcemap: true,
    },
  };
});
