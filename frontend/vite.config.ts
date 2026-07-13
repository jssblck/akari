import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
  base: "/app-assets/",
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
