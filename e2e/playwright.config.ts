import { defineConfig, devices } from '@playwright/test';

// Chromium (the engine the design measured Sec-Fetch-Dest in). The
// project's viewport is a desktop's; a spec about another size sets its own
// (contacts.spec.ts runs a phone's, 390x844, and a desktop's, 1280x800).
// forbidOnly and retries 0: a flaky gate must show as red, not pass on retry.
// PLAYWRIGHT_CHROMIUM names a Chromium to launch instead of Playwright's own
// download (a workstation whose package manager ships one); CI leaves it
// unset.
const executablePath = process.env.PLAYWRIGHT_CHROMIUM || undefined;

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  forbidOnly: true,
  retries: 0,
  timeout: 120_000, // the first `go run` of the harness compiles it
  reporter: [['list']],
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], launchOptions: { executablePath } } }],
});
