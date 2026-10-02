// Captures docs/screenshots/*.png from a running oscwebmixer instance.
// Requires a real desk connection: the mixer's level/pan views only render
// once the server reaches status-healthy, which needs actual desk traffic.
const { chromium } = require('playwright');

const BASE_URL = process.env.BASE_URL || 'http://host.docker.internal:8080';
const OUT_DIR = process.env.OUT_DIR || '/output';

const ADMIN_TABS = [
  ['network', 'oscwebmixer-go_admin_1_network.png'],
  ['external-devices', 'oscwebmixer-go_admin_2_devices.png'],
  ['sources', 'oscwebmixer-go_admin_3_sources.png'],
  ['auxes', 'oscwebmixer-go_admin_4_auxes.png'],
  ['presets', 'oscwebmixer-go_admin_5_presets.png'],
  ['settings', 'oscwebmixer-go_admin_6_settings.png'],
];

const LANDSCAPE = { width: 1440, height: 900, deviceScaleFactor: 2 };
const PORTRAIT = { width: 420, height: 860, deviceScaleFactor: 3 };

async function shot(page, name) {
  await page.screenshot({ path: `${OUT_DIR}/${name}` });
  console.log('saved', name);
}

async function openAuxPicker(page) {
  await page.goto(BASE_URL, { waitUntil: 'networkidle' });
  await page.waitForSelector('body.picking-aux #aux-picker', { timeout: 15000 });
}

// Clicks through aux buttons (picker must already be open) until one with a
// stereo pan toggle is found. Returns true if a stereo aux was selected.
async function selectStereoAux(page) {
  const auxButtons = await page.locator('.aux-button').all();
  if (auxButtons.length === 0) {
    throw new Error('no aux buttons found — is a desk connected and config populated?');
  }

  for (const btn of auxButtons) {
    if (!(await page.locator('body.picking-aux').count())) {
      await page.click('#aux-trigger');
    }
    await btn.click();
    await page.waitForSelector('#channels', { state: 'visible', timeout: 15000 });
    if (await page.locator('#pan-toggle-wrap:not([hidden])').count()) {
      return true;
    }
  }
  return false;
}

async function ensureLevelView(page) {
  if (await page.locator('#pan-toggle').count() && (await page.isChecked('#pan-toggle'))) {
    await page.click('#pan-toggle');
  }
}

// The container healthcheck only proves the HTTP server is up, not that the
// desk handshake finished — poll the admin panel's own status dot instead.
async function waitForDeskHealthy(timeoutMs = 120000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${BASE_URL}/admin/network/status`);
      if (res.ok && (await res.text()).includes('data-status="healthy"')) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error('desk never reached healthy status within timeout — check the console connection');
}

(async () => {
  await waitForDeskHealthy();

  const browser = await chromium.launch();

  const adminPage = await browser.newPage({ viewport: LANDSCAPE, deviceScaleFactor: LANDSCAPE.deviceScaleFactor });
  await adminPage.goto(`${BASE_URL}/admin`, { waitUntil: 'networkidle' });
  for (const [hash, file] of ADMIN_TABS) {
    await adminPage.click(`a[href="#${hash}"]`);
    await adminPage.waitForSelector(`#tab-${hash}.show`, { timeout: 5000 });
    await shot(adminPage, file);
  }
  await adminPage.close();

  // iPad Pro 11" landscape: aux select + level/pan
  const landscapeCtx = await browser.newContext({ viewport: LANDSCAPE, deviceScaleFactor: LANDSCAPE.deviceScaleFactor });
  const landscapePage = await landscapeCtx.newPage();
  await openAuxPicker(landscapePage);
  await shot(landscapePage, 'oscwebmixer-go_mixer_auxselect_landscape.png');
  const landscapeStereo = await selectStereoAux(landscapePage);
  await ensureLevelView(landscapePage);
  await shot(landscapePage, 'oscwebmixer-go_mixer_level_landscape.png');
  if (landscapeStereo) {
    await landscapePage.check('#pan-toggle');
    await shot(landscapePage, 'oscwebmixer-go_mixer_pan_landscape.png');
  } else {
    console.warn('no stereo aux found in config — skipped pan_landscape');
  }
  await landscapeCtx.close();

  // iPhone 15 portrait: level/pan (fresh context, so re-pick the aux)
  const portraitCtx = await browser.newContext({ viewport: PORTRAIT, deviceScaleFactor: PORTRAIT.deviceScaleFactor });
  const portraitPage = await portraitCtx.newPage();
  await openAuxPicker(portraitPage);
  await shot(portraitPage, 'oscwebmixer-go_mixer_auxselect_portrait.png');
  const portraitStereo = await selectStereoAux(portraitPage);
  await ensureLevelView(portraitPage);
  await shot(portraitPage, 'oscwebmixer-go_mixer_level_portrait.png');
  if (portraitStereo) {
    await portraitPage.check('#pan-toggle');
    await shot(portraitPage, 'oscwebmixer-go_mixer_pan_portrait.png');
  } else {
    console.warn('no stereo aux found in config — skipped pan_portrait');
  }
  await portraitCtx.close();

  await browser.close();
})();
