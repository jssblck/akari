import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
  // Relative base: chunk-to-chunk resolution rides import.meta.url in the
  // build, and the Go server rewrites the entry document's own references to
  // the (possibly path-prefixed) /app-assets mount per request. An absolute
  // base here would bake the mount into the bundle and break deployments
  // behind a path prefix. See internal/server/frontend/embed.go.
  base: "./",
  plugins: [react()],
  build: {
    outDir: "../internal/server/frontend/dist",
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8080",
      "/static": "http://127.0.0.1:8080",
      "/sessions": "http://127.0.0.1:8080",
    },
  },
});
