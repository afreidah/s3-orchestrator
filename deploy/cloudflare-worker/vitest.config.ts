// -------------------------------------------------------------------------------
// Vitest Configuration - Edge Proxy Worker
//
// Author: Alex Freidah
//
// Runs the worker suite on the Node runtime rather than a Workers pool. The
// signing path depends only on Web Crypto, Request, Headers and Response, all
// of which Node provides as globals, so the heavier pool buys nothing here.
//
// Thresholds fail the run rather than only reporting, so the suite cannot rot
// quietly alongside a change to the signing path.
// -------------------------------------------------------------------------------

import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
    coverage: {
      provider: "v8",
      include: ["src/**/*.ts"],
      exclude: ["src/**/*.test.ts"],
      reporter: ["text", "lcov"],
      thresholds: {
        statements: 90,
        branches: 85,
        functions: 90,
        lines: 90,
      },
    },
  },
});
