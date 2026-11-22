// The Contacts tab in a real browser, framed by a muster-like parent as
// muster frames it, at a phone's size (390x844) and a desktop's (1280x800):
// the list, search, a contact's sheet with its meeting history, Edit, Add
// and Delete; then the empty state, Download all, reduced motion and the
// UI kit. Cards arrive as the meetings plugin sends them (the harness posts
// to the contacts API under the subject Meetings knows the person by), and
// every one is sent BEFORE the person first opens Contacts.
import { test, expect, type FrameLocator } from '@playwright/test';
import { readFile } from 'node:fs/promises';
import { startHarness, seedCard, openTab, kitMode, sizes, type Harness } from './helpers';

let h: Harness;

test.beforeAll(async () => { h = await startHarness(); });
test.afterAll(() => { h?.proc.stdin?.end(); });

function rows(app: FrameLocator) { return app.locator('#contacts li.contact'); }
function row(app: FrameLocator, name: string) { return rows(app).filter({ has: app.locator('.contact__name', { hasText: name }) }); }

for (const size of sizes) {
  const person = size.name === 'mobile' ? 'ada' : 'cy';
  test(`${size.width}x${size.height}: list, search, detail, edit, add, delete`, async ({ browser, request }) => {
    const grace = `grace.${size.name}@example.test`, alan = `alan.${size.name}@example.test`;
    await seedCard(request, h, person, { name: 'Grace Hopper', email: grace, meeting: 'mWeekly' + size.width, title: 'Weekly sync' });
    await seedCard(request, h, person, { name: 'Alan Turing', email: alan, meeting: 'mPlan' + size.width, title: 'Planning' });
    const ctx = await browser.newContext({ viewport: { width: size.width, height: size.height } });
    const page = await ctx.newPage();
    const app = await openTab(page, h, person);

    // The list: newest first, both marked as added by them.
    await expect(rows(app)).toHaveCount(2);
    await expect(rows(app).nth(0).locator('.contact__name')).toHaveText('Alan Turing');
    await expect(rows(app).locator('.origin', { hasText: 'Added by them' })).toHaveCount(2);

    // Search filters on the server as you type.
    await app.locator('#q').fill('grace');
    await expect(rows(app)).toHaveCount(1);
    await expect(rows(app).locator('.contact__name')).toHaveText('Grace Hopper');
    await app.locator('#q').fill('nobody-matches-this');
    await expect(app.locator('#empty')).toBeVisible();
    await expect(app.locator('#empty-text')).toHaveText('No one matches.');
    await app.locator('#q').fill('grace');
    await expect(rows(app)).toHaveCount(1);

    // The sheet: emails (not confirmed), the meeting history, provenance.
    await row(app, 'Grace Hopper').locator('button').click();
    const sheet = app.locator('#sheet');
    await expect(sheet).toBeVisible();
    await expect(app.locator('#sheet-title')).toHaveText('Grace Hopper');
    await expect(app.locator('#d-emails .address')).toHaveText(grace);
    await expect(app.locator('#d-emails .mu-chip')).toHaveText('not confirmed');
    const met = app.locator('#d-meetings li');
    await expect(met).toHaveCount(1);
    await expect(met.locator('.line__title')).toHaveText('Weekly sync');
    await expect(met.locator('.mu-chip')).toHaveText('email not confirmed');
    await expect(met.locator('a')).toHaveAttribute('href', `${h.parent}/apps/${h.meetingsID}?ref=mWeekly${size.width}`);
    await expect(met.locator('a')).toHaveAttribute('target', '_blank');
    await expect(app.locator('#d-origin li')).toHaveText(['Added by them after Weekly sync']);
    // A bottom sheet on a phone, a centred dialog on a desktop.
    const box = (await sheet.boundingBox())!;
    if (size.width < 640) expect(box.width).toBeGreaterThan(size.width - 2);
    else expect(box.width).toBeLessThan(size.width * 0.6);

    // Edit: the name and a note; Save; the list follows.
    await app.locator('#btn-edit').click();
    await expect(app.locator('#edit')).toBeVisible();
    await app.locator('#f-name').fill('Grace B. Hopper');
    await app.locator('#f-note').fill('Asked about compilers.');
    await app.locator('#btn-save').click();
    await expect(app.locator('#view')).toBeVisible();
    await expect(app.locator('#sheet-title')).toHaveText('Grace B. Hopper');
    await expect(app.locator('#d-note')).toHaveText('Asked about compilers.');
    await page.keyboard.press('Escape');
    await expect(sheet).toBeHidden();
    await expect(rows(app).locator('.contact__name')).toHaveText('Grace B. Hopper');
    await app.locator('#q').fill('');
    await expect(rows(app)).toHaveCount(2);

    // Add: a contact typed by the owner, with no "Added by them".
    await app.locator('#btn-add').click();
    await expect(app.locator('#sheet-title')).toHaveText('New contact');
    await app.locator('#f-name').fill('Katherine Johnson');
    await app.locator('#f-emails input').first().fill(`katherine.${size.name}@example.test`);
    await app.locator('#btn-save').click();
    await expect(app.locator('#sheet-title')).toHaveText('Katherine Johnson');
    await expect(app.locator('#d-origin li')).toHaveText(['Added by you']);
    await page.keyboard.press('Escape');
    await expect(rows(app)).toHaveCount(3);
    await expect(rows(app).nth(0).locator('.contact__name')).toHaveText('Katherine Johnson');
    await expect(row(app, 'Katherine Johnson').locator('.origin')).toHaveCount(0);

    // Delete, behind a confirmation; gone after a reload too.
    await row(app, 'Alan Turing').locator('button').click();
    await app.locator('#btn-delete').click();
    await expect(app.locator('#confirm')).toBeVisible();
    await expect(app.locator('#confirm-title')).toHaveText('Delete Alan Turing?');
    await app.locator('#confirm-delete').click();
    await expect(app.locator('#confirm')).toBeHidden();
    await expect(row(app, 'Alan Turing')).toHaveCount(0);
    const again = await openTab(page, h, person);
    await expect(rows(again)).toHaveCount(2);
    await expect(row(again, 'Alan Turing')).toHaveCount(0);
    await ctx.close();
  });
}

test('another person sees none of it, and a first open shows the cards sent before it', async ({ page, request }) => {
  const app = await openTab(page, h, 'bea');
  await expect(rows(app)).toHaveCount(0);
  await expect(app.locator('#empty-text')).toHaveText('People you meet in Meetings appear here.');
  await seedCard(request, h, 'dee', { name: 'Margaret Hamilton', email: 'margaret@example.test', meeting: 'mFirst', title: 'Kickoff' });
  const dee = await openTab(page, h, 'dee');
  await expect(rows(dee).locator('.contact__name')).toHaveText(['Margaret Hamilton']);
});

test('Escape leaves the form, not the sheet; a held email offers Merge', async ({ page, request }) => {
  await seedCard(request, h, 'dee', { name: 'Mary Somerville', email: 'mary@example.test', meeting: 'mEsc0', title: 'Retro' });
  await seedCard(request, h, 'dee', { name: 'Hedy Lamarr', email: 'hedy@example.test', meeting: 'mEsc', title: 'Standup' });
  const app = await openTab(page, h, 'dee');
  await row(app, 'Hedy Lamarr').locator('button').click();
  await app.locator('#btn-edit').click();
  await app.locator('#f-name').fill('Changed but cancelled');
  await page.keyboard.press('Escape');
  await expect(app.locator('#sheet')).toBeVisible();
  await expect(app.locator('#view')).toBeVisible();
  await expect(app.locator('#sheet-title')).toHaveText('Hedy Lamarr');
  // Another contact holds mary@: saving offers Merge, which merges.
  await app.locator('#btn-edit').click();
  await app.locator('#f-add-email').click();
  await app.locator('#f-emails input').last().fill('Mary@example.test');
  await app.locator('#btn-save').click();
  await expect(app.locator('#f-merge')).toBeVisible();
  await expect(app.locator('#f-merge-text')).toHaveText('Mary Somerville already has this email.');
  await app.locator('#f-merge-btn').click();
  await expect(app.locator('#view')).toBeVisible();
  // Both addresses, in the order they arrived, the merged one as typed.
  await expect(app.locator('#d-emails .address')).toHaveText(['Mary@example.test', 'hedy@example.test']);
  await expect(app.locator('#d-meetings li')).toHaveCount(2);
  await page.keyboard.press('Escape');
  await expect(row(app, 'Mary Somerville')).toHaveCount(0);
});

test('Download all holds every contact', async ({ page, request }) => {
  await seedCard(request, h, 'dee', { name: 'Ada Export', email: 'export.fixture@example.test', meeting: 'mExp', title: 'Export' });
  const app = await openTab(page, h, 'dee');
  await app.locator('#btn-settings').click();
  await expect(app.locator('#settings')).toBeVisible();
  const [dl] = await Promise.all([page.waitForEvent('download'), app.locator('#s-download').click()]);
  expect(dl.suggestedFilename()).toBe('contacts.json');
  const body = JSON.parse(await readFile((await dl.path())!, 'utf8'));
  expect(body.contacts.map((c: any) => c.emails[0].address)).toContain('export.fixture@example.test');
});

test('reduced motion: the list and the sheet move at once', async ({ browser, request }) => {
  await seedCard(request, h, 'dee', { name: 'Motion Check', email: 'motion@example.test', meeting: 'mMot', title: 'Motion' });
  for (const motion of ['no-preference', 'reduce'] as const) {
    const ctx = await browser.newContext({ reducedMotion: motion });
    const page = await ctx.newPage();
    const app = await openTab(page, h, 'dee');
    const rowDur = await rows(app).first().evaluate((el) => getComputedStyle(el).animationDuration);
    await rows(app).first().locator('button').click();
    await expect(app.locator('#sheet')).toBeVisible();
    const sheetDur = await app.locator('#sheet').evaluate((el) => getComputedStyle(el).animationDuration);
    if (motion === 'reduce') {
      expect(rowDur, 'a row under reduced motion').toBe('0s');
      expect(sheetDur, 'the sheet under reduced motion').toBe('0s');
    } else {
      // The control: without it, both animate (so the check can fail).
      expect(rowDur).not.toBe('0s');
      expect(sheetDur).not.toBe('0s');
    }
    await ctx.close();
  }
});

test('the kit: the theme flows through, and a failing kit keeps the page working', async ({ page, request }) => {
  const accent = async (app: FrameLocator) => app.locator('body').evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim());
  await kitMode(request, h, 'theme=harbor');
  const harbor = await accent(await openTab(page, h, 'bea'));
  await kitMode(request, h, 'theme=brass');
  const brass = await accent(await openTab(page, h, 'bea'));
  expect(harbor).not.toBe(brass);
  // muster's kit fails: the plugin serves the last copy it fetched (the
  // bundled kit when it has none: internal/server kit_internal_test.go), so
  // the page still draws in the install's theme and kit.js still works.
  await kitMode(request, h, 'missing=css,js');
  const app = await openTab(page, h, 'bea');
  expect(await accent(app)).toBe(brass);
  await app.locator('#btn-add').click();
  await expect(app.locator('#sheet')).toBeVisible();
  await kitMode(request, h, 'theme=brass');
});

test('a top-level /enter is refused with no redeem, and the same code works in muster\'s frame', async ({ page, request }) => {
  const code = await (await request.get(`${h.parent}/mint?person=bea`)).text();
  const before = Number(await (await request.get(`${h.parent}/redeems`)).text());
  const resp = await page.goto(`${h.plugin}/enter?muster_code=${encodeURIComponent(code)}&muster_plugin=${h.pluginID}`);
  expect(resp?.status()).toBe(403);
  await expect(page.locator('#message')).toHaveText('Open Contacts from muster.');
  expect(Number(await (await request.get(`${h.parent}/redeems`)).text())).toBe(before);
  await page.goto(`${h.parent}/apps/${h.pluginID}?person=bea&code=${encodeURIComponent(code)}`);
  await expect(page.frameLocator('#app').locator('#list-view')).toBeVisible();
});
