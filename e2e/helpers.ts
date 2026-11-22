// Shared by the specs: the harness, a guest's card as meetings sends it, and
// the owner's way into the Contacts tab.
import { expect, type APIRequestContext, type FrameLocator, type Page } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

export type Harness = { proc: ChildProcess; plugin: string; parent: string; pluginID: number; meetingsID: number };

export async function startHarness(): Promise<Harness> {
  const proc = spawn('go', ['run', './scripts/e2e-harness'], {
    cwd: path.resolve(__dirname, '..'),
    stdio: ['pipe', 'pipe', 'inherit'],
  });
  const line = await new Promise<string>((resolve, reject) => {
    const rl = createInterface({ input: proc.stdout! });
    rl.once('line', resolve);
    proc.once('exit', (code) => reject(new Error(`the harness exited ${code} before it started`)));
  });
  const j = JSON.parse(line);
  return { proc, plugin: j.plugin, parent: j.parent, pluginID: j.plugin_id, meetingsID: j.meetings_id };
}

export type Card = { name: string; email: string; meeting: string; title: string };

// seedCard sends a guest's card to the contacts API as meetings sends it,
// under the subject Meetings knows person by (the linked pair's).
export async function seedCard(request: APIRequestContext, h: Harness, person: string, c: Card) {
  const q = new URLSearchParams({ person, ...c });
  const r = await request.post(`${h.parent}/seed-card?${q}`);
  expect(r.status(), await r.text()).toBe(202);
}

export async function kitMode(request: APIRequestContext, h: Harness, q: string) {
  const r = await request.get(`${h.parent}/kit-mode?${q}`);
  expect(r.status(), await r.text()).toBe(200);
}

// openTab opens muster's page for person: the Contacts tab in its frame,
// with the list loaded.
export async function openTab(page: Page, h: Harness, person: string): Promise<FrameLocator> {
  await page.goto(`${h.parent}/apps/${h.pluginID}?person=${person}`);
  const app = page.frameLocator('#app');
  await expect(app.locator('#list-view')).toBeVisible();
  await expect(app.locator('#contacts li, #empty:not([hidden])').first()).toBeVisible();
  return app;
}

export const sizes = [{ name: 'mobile', width: 390, height: 844 }, { name: 'desktop', width: 1280, height: 800 }];
