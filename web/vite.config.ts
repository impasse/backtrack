import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [
    sveltekit({
      alias: { '#lib': './src/lib' },
      adapter: adapter({ pages: 'build', assets: 'build', fallback: '200.html', strict: false }),
    }),
  ],
  server: { proxy: { '/api': 'http://localhost:8080' } }
});
