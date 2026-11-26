import { mergeConfig } from "vite";
import { defineConfig } from "vitest/config";
import viteConfig from "./vite.config.ts";

export default mergeConfig(
  viteConfig,
  defineConfig({
    // Shared cases with Go: the ⌘K contract's in webui/testdata, the plain body's in core/mail/testdata.
    server: { fs: { allow: [".", "../webui/testdata", "../core/mail/testdata"] } },
    test: {
      environment: "jsdom",
      globals: true,
      setupFiles: ["./src/test-setup.ts"],
      css: { include: [/frame\.css/] },
    },
  }),
);
