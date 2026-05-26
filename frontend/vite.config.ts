import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    // Allow CSS to @import design-system/ at the repo root (one level above
    // the frontend/ project root). Without this, Vite's dev fs sandbox
    // refuses reads outside the project root.
    fs: { allow: [".."] },
    proxy: {
      "/v1": "http://localhost:8000",
      "/healthz": "http://localhost:8000",
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    rollupOptions: {
      output: {
        manualChunks: {
          "vendor-flow": ["@xyflow/react"],
          "vendor-react": ["react", "react-dom", "react-router-dom"],
        },
      },
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: "./src/test/setup.ts",
  },
});
