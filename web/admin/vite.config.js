import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The built dist is served by the Go admin (claude-cron serve) at /app/ and
// embedded into the binary via go:embed, so it must live under the channelagent
// package dir. Relative base makes hashed asset URLs resolve under /app/.
export default defineConfig({
  base: './',
  plugins: [svelte()],
  build: {
    outDir: '../../internal/channelagent/admin_dist',
    emptyOutDir: true,
  },
})
