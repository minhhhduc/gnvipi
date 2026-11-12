import { defineConfig } from "vite";
import preact from "@preact/preset-vite";
import tailwindcss from "@tailwindcss/vite";

// The Go binary embeds exactly index.html, app.js and app.css. Keep the
// filenames fixed and forbid hashed chunks or extra assets.
export default defineConfig({
  root: "src",
  base: "/admin/",
  plugins: [preact(), tailwindcss()],
  build: {
    outDir: "../dist",
    emptyOutDir: true,
    cssCodeSplit: false,
    modulePreload: { polyfill: false },
    rollupOptions: {
      output: {
        inlineDynamicImports: true,
        entryFileNames: "app.js",
        chunkFileNames: "app.js",
        assetFileNames: (asset) =>
          asset.names?.some((n) => n.endsWith(".css")) ? "app.css" : "app.[ext]",
      },
    },
  },
  test: {
    include: ["**/*.test.ts"],
    environment: "node",
  },
});
