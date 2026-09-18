#!/usr/bin/env node
// Prism web UI smoke check (docker/onyx-web/index.html).
//
// The UI is one hand-written inline script with no build step, so a typo or a
// bad edit is only caught by a browser. This runs that script for real in a VM
// with a minimal DOM stub and asserts the behaviour that has actually broken
// before: capacity rendering for a visible pool, a pool that is mounted but
// invisible to onyx-api, a host with no storage, the presence of the cloud
// provider configuration in the Shares tab, and the v0.4 platform pages (apps,
// VMs, objects) actually rendering what the API returns.
//
// Run directly (`node scripts/web-ui-check.js`) or through `make check`.
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const uiPath = path.join(__dirname, '..', 'docker', 'onyx-web', 'index.html');
const html = fs.readFileSync(uiPath, 'utf8');

const start = html.indexOf('<script>');
const end = html.indexOf('</script>', start);
if (start < 0 || end < 0) {
  console.error('web-ui-check: no inline <script> found in ' + uiPath);
  process.exit(1);
}
const code = html.slice(start + '<script>'.length, end);

// The app is an IIFE; expose the functions under test before it closes.
const hook =
  '\n    globalThis.__onyx = { loadStorageOverview: loadStorageOverview, formatBytes: formatBytes,' +
  ' viewShares: viewShares, viewStorage: viewStorage, viewFiles: viewFiles,' +
  ' viewApps: viewApps, viewVMs: viewVMs, viewObjects: viewObjects, loadBuckets: loadBuckets,' +
  ' views: VIEWS };\n';
const instrumented = code.replace(/\}\)\(\);\s*$/, hook + '  })();\n');

function element(id) {
  return {
    id: id || '', innerHTML: '', textContent: '', value: '', checked: false,
    disabled: false, style: {}, dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; } },
    addEventListener() {}, querySelectorAll: () => [], querySelector: () => null,
    appendChild() {}, insertAdjacentHTML() {}, focus() {}, click() {},
  };
}

const elements = {};
const store = {};
let payloadFor = () => ({});

const sandbox = {
  console: { log() {}, warn() {}, error() {} },
  document: {
    getElementById: (id) => (elements[id] || (elements[id] = element(id))),
    querySelectorAll: () => [], querySelector: () => null,
    documentElement: { dataset: {} }, addEventListener() {}, createElement: element, body: element(),
  },
  window: {
    location: { hash: '', hostname: 'localhost' }, addEventListener() {}, open() {}, prompt() {},
    matchMedia: () => ({ matches: false, addEventListener() {} }),
  },
  location: { hash: '', hostname: 'localhost', reload() {} },
  localStorage: {
    getItem: (k) => (k in store ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: (k) => { delete store[k]; },
  },
  fetch: (url) => {
    const target = String(url).replace(/^.*\/api\/v1/, '');
    const body = payloadFor(target);
    if (body === undefined) {
      return Promise.resolve({ ok: false, json: () => Promise.resolve({ error: { message: 'no stub for ' + target } }) });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve(body) });
  },
  setTimeout, clearTimeout, setInterval: () => 0, clearInterval: () => {},
  requestAnimationFrame: (fn) => setTimeout(fn, 0),
  Promise, Date, Math, Number, String, Object, Array, JSON,
  parseInt, parseFloat, isNaN, encodeURIComponent, decodeURIComponent,
  FormData: class {}, alert() {}, confirm: () => true, navigator: {},
};
sandbox.globalThis = sandbox;
vm.createContext(sandbox);

try {
  vm.runInContext(instrumented, sandbox, { filename: 'onyx-web-inline.js' });
} catch (err) {
  // The bootstrap paints the initial route; a stub DOM can refuse parts of it
  // without invalidating the functions under test.
  console.error('web-ui-check: bootstrap threw: ' + err.message);
}
if (!sandbox.__onyx) {
  console.error('web-ui-check: the inline script did not define its functions (syntax error?)');
  process.exit(1);
}
const ui = sandbox.__onyx;
const el = (id) => (elements[id] || (elements[id] = element(id)));

const failures = [];
function check(name, ok, detail) {
  if (ok) return;
  failures.push(name + (detail ? ': ' + detail : ''));
}

const storageCard = el('storage-card');
const warning = el('file-storage-warning');

const cases = [
  {
    name: 'visible pool shows its own capacity',
    payload: {
      storage_root: '/mnt/onyx', warnings: [],
      primary: { name: 'main-pool', total_bytes: 2199023255552, free_bytes: 103079215104 },
      entries: [{ name: 'main-pool', visible: true, mounted: true }],
    },
    expect: (card, warn) =>
      card.includes('2.00 TB') && card.includes('main-pool') && card.includes('96.00 GB free') && warn === '',
  },
  {
    name: 'a mounted but invisible pool is not reported as "no storage"',
    payload: {
      storage_root: '/mnt/onyx',
      warnings: ['main-pool is mounted at /mnt/onyx/main-pool by the data plane but is not visible to onyx-api: the mount namespace is not shared. Run `mount --make-shared /mnt/onyx` on the host and restart the stack.'],
      primary: null,
      entries: [{ name: 'main-pool', visible: false, mounted: true }],
    },
    expect: (card, warn) => card.includes('Mounted') && warn.includes('mount --make-shared'),
  },
  {
    name: 'no storage is reported plainly',
    payload: {
      storage_root: '/mnt/onyx',
      warnings: ['No storage is mounted under /mnt/onyx. Create a pool from the Storage page or attach a device.'],
      primary: null, entries: [],
    },
    expect: (card, warn) => card.includes('No pool mounted') && warn.includes('No storage is mounted'),
  },
];

(async () => {
  for (const c of cases) {
    payloadFor = (target) => (target === '/storage/overview' ? c.payload : {});
    storageCard.innerHTML = '';
    warning.innerHTML = '';
    await ui.loadStorageOverview();
    check(c.name, c.expect(storageCard.innerHTML, warning.innerHTML),
      'card=' + storageCard.innerHTML + ' warning=' + warning.innerHTML);
  }

  const shares = ui.viewShares();
  check('Shares tab configures cloud providers', shares.includes('id="share-remote"'));
  check('Shares tab has the provider picker', shares.includes('id="remote-provider"'));
  check('Shares tab can clone to a target', shares.includes('id="remote-clone"'));
  check('cloud configuration sits above the share list',
    shares.indexOf('id="share-remote"') < shares.indexOf('id="share-list"'));

  const storage = ui.viewStorage();
  check('Storage page has a capacity card', storage.includes('id="storage-card"'));
  check('Storage page has a working refresh control', storage.includes('id="storage-refresh"'));
  check('Files page has room for the storage notice', ui.viewFiles().includes('id="file-storage-warning"'));

  // v0.4 "Jade" platform pages. The Apps page used to be a placeholder saying
  // the app store would land later; these checks are what keep it from quietly
  // becoming one again when someone reverts a render branch.
  const apps = ui.viewApps();
  check('Apps page renders the catalog', apps.includes('id="app-catalog"'));
  check('Apps page renders installed apps', apps.includes('id="app-installed"'));
  check('Apps page renders containers', apps.includes('id="app-containers"'));
  check('Apps page is no longer a placeholder', !apps.includes('land in v0.4'));

  const vms = ui.viewVMs();
  check('VMs page has a create form', vms.includes('id="vm-create"'));
  check('VMs page lists machines', vms.includes('id="vm-list"'));

  const objects = ui.viewObjects();
  check('Objects page creates buckets', objects.includes('id="bucket-create"'));
  check('Objects page offers configured remotes as targets', objects.includes('id="bucket-remotes"'));
  check('Objects page lists buckets', objects.includes('id="bucket-list"'));

  const navIDs = (ui.views || []).map((view) => view.id);
  check('the nav offers the VMs and Objects pages', navIDs.includes('vms') && navIDs.includes('objects'), 'ids=' + navIDs);

  // A tiered bucket has to show the remote it mirrors to and its eviction
  // policy; a local bucket must not claim a cloud target it does not have.
  const bucketCases = [
    {
      name: 'a tiered bucket shows its target and eviction age',
      payload: {
        buckets: [{
          name: 'documents', tier: 'TIERED', cloudTarget: 'b2-archive:',
          localObjects: '4', cloudObjects: '4', lastSyncAt: '2026-09-18T01:00:00Z', evictAfterDays: 30,
        }],
      },
      expect: (htmlOut) =>
        htmlOut.includes('documents') && htmlOut.includes('b2-archive:') &&
        htmlOut.includes('evict after 30 days') && htmlOut.includes('4 objects in the cloud') &&
        htmlOut.includes('data-bucket-sync=') && htmlOut.includes('data-bucket-evict='),
    },
    {
      name: 'a local bucket shows no cloud target',
      payload: { buckets: [{ name: 'local-data', tier: 'LOCAL', localObjects: '1' }] },
      expect: (htmlOut) =>
        htmlOut.includes('local-data') && htmlOut.includes('1 object local') &&
        !htmlOut.includes('data-bucket-sync=') && !htmlOut.includes('data-bucket-evict=') &&
        !htmlOut.includes('in the cloud'),
    },
  ];
  for (const c of bucketCases) {
    payloadFor = (target) => (target === '/buckets' ? c.payload : target === '/storage/remotes' ? { remotes: ['b2-archive'] } : {});
    const list = el('bucket-list');
    list.innerHTML = '';
    await ui.loadBuckets();
    check(c.name, c.expect(list.innerHTML), 'list=' + list.innerHTML);
  }

  if (failures.length) {
    console.error('web-ui-check: ' + failures.length + ' check(s) failed');
    for (const f of failures) console.error('  - ' + f);
    process.exit(1);
  }
  process.stdout.write('web-ui-check: ok (' + (cases.length + bucketCases.length) + ' render cases + 17 structure checks)\n');
})();
