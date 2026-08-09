/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The control-plane API this UI reads. Same-origin in production (the Go
// binary will serve `dist/` alongside `/v1alpha1` in a later task — see
// README.md), proxied in dev so the browser never needs CORS.
const API_TARGET = process.env.NODES_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/v1alpha1": {
        target: API_TARGET,
        changeOrigin: true,
      },
    },
  },
  preview: {
    host: "127.0.0.1",
    port: 4173,
    strictPort: true,
  },
  build: {
    outDir: "dist",
    sourcemap: true,
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./vitest.setup.ts"],
    css: false,
    // e2e/ is Playwright's; vitest must not try to run it.
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
