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
  ' loadUsers: loadUsers, viewUsers: viewUsers, userSambaPanel: userSambaPanel,' +
  ' shareAccessPanel: shareAccessPanel, shareAuditPanel: shareAuditPanel,' +
  ' userCleanupWarnings: userCleanupWarnings,' +
  ' activeViews: activeViews, commands: commands, viewDashboard: viewDashboard, loadStatus: loadStatus,' +
  ' viewSettings: viewSettings, render: render, loadSettingsRemotes: loadSettingsRemotes,' +
  ' renderRemoteFields: renderRemoteFields, collectRemoteParams: collectRemoteParams, continueRemoteSetup: continueRemoteSetup,' +
  ' views: VIEWS };\n';
const instrumented = code.replace(/\}\)\(\);\s*$/, hook + '  })();\n');

function element(id) {
  return {
    id: id || '', innerHTML: '', textContent: '', value: '', checked: false,
    disabled: false, style: {}, dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; } },
    listeners: {}, addEventListener(event, handler) { this.listeners[event] = handler; }, querySelectorAll: () => [], querySelector: () => null,
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
  fetch: (url, options) => {
    const target = String(url).replace(/^.*\/api\/v1/, '');
    const body = payloadFor(target, options);
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
let checkCount = 0;
function check(name, ok, detail) {
  checkCount++;
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

  const nasViews = ui.activeViews().map((view) => view.id);
  check('NAS focuses on disks, shares and backups', ['storage', 'files', 'shares', 'backups', 'snapshots'].every((id) => nasViews.includes(id)));
  check('NAS hides application and VM pages', ['apps', 'vms', 'objects'].every((id) => !nasViews.includes(id)));
  check('NAS command palette follows the selected profile', !ui.commands().some((command) => command.label === 'Go to Apps'));
  check('NAS dashboard guides disk attachment and sharing', ui.viewDashboard().includes('Attach an existing disk') && ui.viewDashboard().includes('#/shares'));
  sandbox.window.ONYX_MODE = 'platform';
  check('Platform mode retains optional services', ui.activeViews().some((view) => view.id === 'apps') && ui.commands().some((command) => command.label === 'Go to VMs'));
  sandbox.window.ONYX_MODE = 'nas';
  const requiredNAS = ['onyx-core', 'onyx-storaged', 'onyx-shared', 'onyx-privd', 'onyx-snapd', 'onyx-backupd'];
  payloadFor = (target) => target === '/system/status' ? { services: requiredNAS.map((name) => ({ name, status: 'SERVING' })).concat([{ name: 'onyx-vmm', status: 'UNKNOWN' }]) } : {};
  await ui.loadStatus();
  check('NAS health excludes optional services', sandbox.window.__onyxServiceHealth.serving === 6 && sandbox.window.__onyxServiceHealth.total === 6);
  payloadFor = (target) => target === '/system/status' ? { services: requiredNAS.slice(1).map((name) => ({ name, status: 'SERVING' })) } : {};
  await ui.loadStatus();
  check('NAS health still detects a missing core service', sandbox.window.__onyxServiceHealth.serving === 5 && sandbox.window.__onyxServiceHealth.total === 6);

  // Settings used to list every backend but submit params: {} for all of
  // them. Exercise the actual create click, not just the field markup.
  const cloudProviders = [
    { type: 'google cloud storage', label: 'Google Cloud Storage', fields: ['service_account_credentials', 'service_account_file', 'project_number'], required: [] },
    { type: 's3', label: 'S3', fields: ['access_key_id', 'secret_access_key'], required: ['access_key_id', 'secret_access_key'] },
    { type: 'iclouddrive', label: 'Apple iCloud Drive', fields: ['apple_id', 'password'], required: ['apple_id', 'password'] },
  ];
  let createBody;
  payloadFor = (target, options) => {
    if (target === '/storage/providers') return { providers: cloudProviders };
    if (target === '/storage/remotes' && options && options.method === 'POST') {
      createBody = JSON.parse(options.body); return { name: createBody.name, type: createBody.type };
    }
    if (target === '/storage/remotes') return { remotes: [] };
    return {};
  };
  ui.render('settings');
  await new Promise((resolve) => setTimeout(resolve, 0));
  check('Settings contains provider credential fields', ui.viewSettings().includes('id="settings-remote-fields"'));
  el('settings-provider').value = 'google cloud storage';
  el('settings-provider').listeners.change();
  check('GCS offers a JSON upload in Settings', el('settings-remote-fields').innerHTML.includes('type="file"') && el('settings-remote-fields').innerHTML.includes('service_account_credentials'));
  const keyJSON = '{"type":"service_account","project_id":"fixture"}';
  el('settings-remote-fields').querySelectorAll = (selector) => selector === '[data-remote-file]'
    ? [{ dataset: { remoteFile: 'service_account_credentials' }, files: [{ size: keyJSON.length, text: () => Promise.resolve(keyJSON) }] }]
    : [{ dataset: { remoteField: 'project_number' }, value: '12345' }];
  el('settings-remote-name').value = 'gcs-fixture';
  el('settings-remote-create').listeners.click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  check('Settings submits uploaded GCS credentials', createBody && createBody.params.service_account_credentials === keyJSON && createBody.params.project_number === '12345');
  check('Settings clears credentials after successful save', el('settings-remote-fields').innerHTML === '');
  el('settings-provider').value = 's3';
  ui.renderRemoteFields(cloudProviders, 'settings-remote');
  check('S3 credentials are visible and password-masked', el('settings-remote-fields').innerHTML.includes('Secret Access Key *') && el('settings-remote-fields').innerHTML.includes('type="password"'));
  el('settings-remote-fields').querySelectorAll = (selector) => selector === '[data-remote-file]' ? [] : [
    { dataset: { remoteField: 'access_key_id' }, value: 'test-access' },
    { dataset: { remoteField: 'secret_access_key' }, value: 'test-secret' },
  ];
  el('settings-remote-name').value = 's3-fixture';
  el('settings-remote-create').listeners.click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  check('Settings submits S3 credentials', createBody.params.access_key_id === 'test-access' && createBody.params.secret_access_key === 'test-secret');
  el('settings-provider').value = 'iclouddrive';
  ui.renderRemoteFields(cloudProviders, 'settings-remote');
  check('iCloud asks for Apple account and password', el('settings-remote-fields').innerHTML.includes('Apple Id *') && el('settings-remote-fields').innerHTML.includes('Password *'));
  ui.continueRemoteSetup('icloud-fixture', { id: 'opaque', question: { name: 'config_2fa', help: 'Enter Apple verification code' } }, 'settings-remote-status');
  check('Apple verification is offered in Settings', el('settings-remote-status').innerHTML.includes('Apple verification code') && el('settings-remote-status').innerHTML.includes('type="password"'));
  ui.continueRemoteSetup('onedrive-fixture', { id: 'opaque', question: { exclusive: true, help: 'Select drive', examples: [{ Value: 'drive123', Help: 'Personal files' }] } }, 'settings-remote-status');
  check('OneDrive offers the provider drive choices', el('settings-remote-status').innerHTML.includes('<select') && el('settings-remote-status').innerHTML.includes('value="drive123"'));

  // --- Shares: who can reach a share, and whether SMB will actually let them ---
  // A grant is rendered into smb.conf's `valid users`, and Samba treats a name
  // with no account of its own as nobody: the share is granted and still
  // refuses them, which is the confusing case the panel has to name.
  payloadFor = (target) => {
    if (target === '/shares/media/access') return { access: [{ share: 'media', username: 'alex', mode: 'read' }], samba_accounts: ['dana'] };
    if (target === '/users') return { users: [{ id: 'u1', username: 'alex' }, { id: 'u2', username: 'dana' }] };
    return {};
  };
  const detail = el('share-detail');
  detail.innerHTML = '';
  await ui.shareAccessPanel('media');
  check('the access panel offers a save', detail.innerHTML.includes('id="share-grant-save"'), detail.innerHTML);
  check('the access panel lists every user', detail.innerHTML.includes('data-share-grant="alex"') && detail.innerHTML.includes('data-share-grant="dana"'), detail.innerHTML);
  check('a grant with no SMB account is called out', detail.innerHTML.includes('SMB sign-in needed') && detail.innerHTML.includes('no SMB account'), detail.innerHTML);
  check('a grant with an account is not called out', (detail.innerHTML.match(/no SMB account/g) || []).length === 1, detail.innerHTML);
  check('the access panel links to the trail', detail.innerHTML.includes('id="share-grant-history"'), detail.innerHTML);

  payloadFor = (target) => (target === '/audit/access?share=media' ? {
    events: [
      { kind: 'denied', username: 'mallory', method: 'PUT', detail: 'PUT not granted', ts: '2026-09-19T10:00:00Z' },
      { kind: 'grant', username: 'dana', mode: 'read-write', actor: 'ada', ts: '2026-09-19T09:00:00Z' },
    ],
  } : {});
  detail.innerHTML = '';
  await ui.shareAuditPanel('media');
  check('the trail shows a refusal with its method', detail.innerHTML.includes('mallory') && detail.innerHTML.includes('refused') && detail.innerHTML.includes('PUT'), detail.innerHTML);
  check('the trail shows a grant with its mode and actor', detail.innerHTML.includes('dana') && detail.innerHTML.includes('read-write') && detail.innerHTML.includes('by ada'), detail.innerHTML);

  // --- Users: the SMB side of an account ---
  payloadFor = (target) => {
    if (target === '/users') return { users: [{ id: 'u1', username: 'alex', role: 'user' }, { id: 'u2', username: 'dana', role: 'user' }], authentik: { enabled: true } };
    if (target === '/samba/accounts') return { available: true, accounts: ['dana'] };
    return {};
  };
  const users = el('user-list');
  users.innerHTML = '';
  await ui.loadUsers();
  check('a user with a Samba account is marked SMB ready', users.innerHTML.includes('SMB ready'), users.innerHTML);
  check('SMB readiness is per user, not per page', (users.innerHTML.match(/SMB ready/g) || []).length === 1, users.innerHTML);
  check('every user can be given an SMB password', (users.innerHTML.match(/SMB password/g) || []).length === 2, users.innerHTML);
  check('an existing SMB account can be removed', users.innerHTML.includes('data-user-smb-remove="u2"'), users.innerHTML);
  check('the Users page has room for the password form', ui.viewUsers().includes('id="user-detail"'));

  // A removal that succeeds can still leave a permission behind. That is the
  // case worth warning about, and a clean removal must stay quiet.
  check('a left-behind SMB account is warned about',
    ui.userCleanupWarnings({ samba_account: 'onyx-core is not reachable' }, 'dana').some((m) => m.includes('still has an SMB account')),
    JSON.stringify(ui.userCleanupWarnings({ samba_account: 'onyx-core is not reachable' }, 'dana')));
  check('uncleared share grants are warned about',
    ui.userCleanupWarnings({ grants_error: 'boom' }, 'dana').some((m) => m.includes('could not be cleared')),
    JSON.stringify(ui.userCleanupWarnings({ grants_error: 'boom' }, 'dana')));
  check('a clean removal warns about nothing',
    ui.userCleanupWarnings({ samba_account: 'removed', grants_removed: 2 }, 'dana').length === 0,
    JSON.stringify(ui.userCleanupWarnings({ samba_account: 'removed', grants_removed: 2 }, 'dana')));
  check('a missing Samba half is not an error',
    ui.userCleanupWarnings({ samba_account: 'skipped' }, 'dana').length === 0,
    JSON.stringify(ui.userCleanupWarnings({ samba_account: 'skipped' }, 'dana')));

  const userDetail = el('user-detail');
  userDetail.innerHTML = '';
  ui.userSambaPanel('u1', 'alex');
  check('the SMB password form asks twice', userDetail.innerHTML.includes('id="smb-password"') && userDetail.innerHTML.includes('id="smb-password-repeat"'), userDetail.innerHTML);

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
  process.stdout.write('web-ui-check: ok (' + (cases.length + bucketCases.length) + ' render cases + ' + checkCount + ' checks)\n');
})();
