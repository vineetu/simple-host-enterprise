'use strict';

const assert = require('node:assert/strict');

const script = process.env.DASHBOARD_SITES_SCRIPT;
if (!script) throw new Error('DASHBOARD_SITES_SCRIPT is empty');

global.__DASHBOARD_TEST__ = true;
global.document = {
  getElementById() { return null; },
  createElement() {
    return { textContent: '', innerHTML: '', style: {} };
  },
};
global.window = global;

new Function(script)();

const api = global.__dashboardSites;
assert.ok(api, 'sites test hook missing');

const LEVELS = ['only_me', 'specific', 'company', 'listed', 'network'];
const ORDER = ['access', 'versions', 'saved', 'files', 'activity', 'showcase', 'download', 'rename', 'move', 'sep', 'delete'];

function ids(site, me, showcase) {
  return api.menuItems(site, me, showcase).map((item) => item.id);
}

const own = {
  owner_username: 'ada',
  name: 'notes',
  access_role: 'owner',
  access: 'only_me',
  active_version: 3,
};
const team = {
  owner_username: 'team-sales',
  name: 'notes',
  access_role: 'member',
  access: 'company',
  active_version: 0,
};

assert.deepEqual(ids(own, 'ada', null), ORDER);
assert.deepEqual(ids(team, 'ada', null), ORDER.filter((id) => id !== 'showcase'));
assert.deepEqual(
  ids({ owner_username: 'ada', name: 'notes', access_role: 'member', access: 'only_me' }, 'ada', null).includes('showcase'),
  false,
);

const pinned = api.menuItems(own, 'ada', { pinned: true });
assert.equal(pinned[pinned.length - 1].id, 'delete');
assert.equal(pinned[pinned.length - 1].danger, true);
assert.equal(pinned[pinned.length - 1].label, 'Delete');
assert.equal(pinned[pinned.length - 2].sep, true);
assert.deepEqual(
  pinned.filter((item) => !item.sep).map((item) => item.label),
  ['Who can open', 'Versions', 'Saved data', 'Uploaded files', 'Activity and visitors', 'Showcase', 'Download', 'Rename', 'Move to a team', 'Delete'],
);
assert.equal(pinned.find((item) => item.id === 'showcase').state, 'pinned');
assert.equal(api.menuItems(own, 'ada', { pinned: false }).find((item) => item.id === 'showcase').state, '');
assert.equal(api.menuItems(own, 'ada', null).find((item) => item.id === 'showcase').state, '');
assert.equal(api.menuItems(own, 'ada').find((item) => item.id === 'versions').state, 'v3 live');
assert.equal(api.menuItems(team, 'ada').find((item) => item.id === 'versions').state, '');

assert.equal(api.accessState({ access: 'only_me' }), 'only you');
assert.equal(api.accessState({ access: 'specific' }), 'named people');
assert.equal(api.accessState({ access: 'company' }), 'company');
assert.equal(api.accessState({ access: 'listed' }), 'listed');
assert.equal(api.accessState({ access: 'network' }), 'network');
assert.equal(api.accessState({ access: 'company', network_request: { approvals: 0 } }), 'company, requested');
assert.equal(api.accessState({
  access: 'only_me',
  network_request: { approvals: 1 },
  access_decision: { decision: 'restricted' },
}), 'only you, restricted');
assert.equal(api.accessState({ access: 'listed', access_decision: { decision: 'declined' } }), 'listed');

for (let i = 0; i < LEVELS.length; i++) {
  for (let j = 0; j < LEVELS.length; j++) {
    assert.equal(api.widensAccess(LEVELS[i], LEVELS[j]), j > i, LEVELS[i] + ' -> ' + LEVELS[j]);
    assert.equal(api.levelReach(LEVELS[i]), i);
  }
}
assert.equal(api.widensAccess('nope', 'network'), false);
assert.equal(api.widensAccess('only_me', 'nope'), false);
assert.equal(api.widensAccess('company', 'company'), false);

assert.equal(api.networkReasonRequired('network'), true);
assert.equal(api.networkReasonRequired('listed'), false);
assert.equal(api.reasonAcceptable(''), false);
assert.equal(api.reasonAcceptable('   '), false);
assert.equal(api.reasonAcceptable('a class needs it'), true);

const widen = api.accessAsk({ owner_username: 'ada', name: 'notes', access: 'only_me' }, 'company');
assert.equal(widen.field, undefined);
assert.equal(widen.confirmLabel, 'Change who can open it');
assert.equal(widen.body, 'Anyone in the company with the link will be able to open it.');

const network = api.accessAsk({ owner_username: 'ada', name: 'notes', access: 'listed' }, 'network');
assert.equal(network.field && network.field.required, true);
assert.equal(network.field.label, 'Why does it need to be open?');
assert.equal(network.confirmLabel, 'Ask an admin');
assert.ok(network.body.includes('An admin must approve this.'));
assert.ok(network.body.includes('Anyone on the network, no sign-in (needs admin approval)'));

const again = api.accessAsk({ owner_username: 'ada', name: 'notes', access: 'network' }, 'network');
assert.equal(again.title, 'Ask for network access for "ada/notes"?');
assert.equal(again.field.required, true);

assert.equal(api.addingViewerWidens('only_me'), true);
assert.equal(api.addingViewerWidens('specific'), false);
assert.equal(api.addingViewerWidens('company'), false);
const viewers = api.viewerAsk({ owner_username: 'ada', name: 'notes' });
assert.equal(viewers.body, 'Adding a viewer changes who can open it to specific people or teams.');
assert.equal(viewers.confirmLabel, 'Add viewers');

assert.equal(api.SEARCH_MIN, 10);
assert.equal(api.searchVisible(0), false);
assert.equal(api.searchVisible(9), false);
assert.equal(api.searchVisible(10), true);
assert.equal(api.searchVisible(11), true);

const sales = { owner_username: 'team-sales', name: 'Roadmap' };
assert.equal(api.siteMatches(sales, ''), true);
assert.equal(api.siteMatches(sales, '   '), true);
assert.equal(api.siteMatches(sales, 'SALES'), true);
assert.equal(api.siteMatches(sales, 'team-sales/roadmap'), true);
assert.equal(api.siteMatches(sales, 'road'), true);
assert.equal(api.siteMatches({ owner_username: 'ada', name: 'notes' }, 'sales'), false);
assert.equal(api.siteMatches({ owner_username: 'Ada', name: 'Notes' }, 'ada/n'), true);
assert.equal(api.siteMatches({ owner_username: 'ada', name: 'x', team_name: 'Platform' }, 'plat'), true);
assert.equal(api.siteMatches({ owner_username: 'ada', name: 'x' }, 'plat'), false);

const dialog = api.deleteDialog('ada', 'notes');
assert.equal(dialog.title, 'Delete "ada/notes"?');
assert.ok(dialog.body.includes('{{DELETED_RETENTION}}'));
assert.ok(dialog.body.includes('Recently deleted'));
assert.ok(dialog.body.includes('gone for good'));
assert.ok(dialog.body.includes('viewers, and uploads'));
assert.equal(dialog.confirmLabel, 'Delete site');
assert.equal(dialog.danger, true);
assert.equal(dialog.field, null);
assert.equal(JSON.stringify(dialog).includes('Type the site name'), false);
assert.equal(JSON.stringify(dialog).toLowerCase().includes('type the'), false);

assert.equal(api.levelLabel('specific'), 'Specific people or teams');
assert.equal(api.LEVELS.length, 5);
