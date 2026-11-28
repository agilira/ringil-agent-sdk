import { existsSync } from "node:fs";
import { defineConfig } from "@playwright/test";
export default defineConfig({
  testDir: "./e2e",
  testMatch: "*.spec.ts",
  workers: 1,
  fullyParallel: false,
  timeout: 300000,
  retries: 0,
  use: {
    ...(existsSync("/usr/bin/google-chrome") ? { channel: "chrome" } : {}),
    baseURL: "http://127.0.0.1:18765",
    trace: "off",
    screenshot: "off",
    video: "off",
    locale: "en-US",
  },
  webServer: {
    command: "node e2e/server.mjs",
    url: "http://127.0.0.1:18765/health",
    reuseExistingServer: false,
    timeout: 120000,
  },
  reporter: "list",
});
