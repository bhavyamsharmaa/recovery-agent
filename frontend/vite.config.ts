import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Tailwind v4 is wired in as a Vite plugin rather than through PostCSS and a
// tailwind.config.js. That is the current standard integration: v4 discovers
// its own content sources and is configured from CSS, so there is no config
// file to keep in step with the source layout.
// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
})
