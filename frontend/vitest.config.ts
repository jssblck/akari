import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// A standalone config rather than merging vite.config.ts: the app config's
// dev proxy and go:embed build output are meaningless under test, and the
// React plugin is the only piece the two configs need to share.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: "happy-dom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    css: false,
    // Pin the suite to a west-of-UTC zone so local-vs-UTC date bugs (the
    // heatmap's "today" boundary) fail everywhere, not only on hosts whose
    // local calendar day happens to trail UTC. A UTC runner would otherwise
    // compute identical dates under both the correct and the buggy code.
    env: { TZ: "America/Anchorage" },
  },
});
