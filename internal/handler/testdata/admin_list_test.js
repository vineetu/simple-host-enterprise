'use strict';

const assert = require('node:assert/strict');

function classList() {
  const set = new Set();
  return {
    set,
    toggle(name, on) { if (on) set.add(name); else set.delete(name); },
    contains(name) { return set.has(name); },
  };
}

function el(props) {
  return Object.assign({
    listeners: {},
    style: {},
    hidden: false,
    attrs: {},
    classList: classList(),
    addEventListener(type, listener) { this.listeners[type] = listener; },
    getAttribute(key) { return this.attrs[key]; },
  }, props);
}

function userBlock(username, sitecount, views, latest, search) {
  return el({ username, attrs: { 'data-username': username, 'data-sitecount': String(sitecount), 'data-views': String(views), 'data-latest': String(latest), 'data-search': search } });
}

function siteRow(name, created, updated, search) {
  return el({ name, attrs: { 'data-created': String(created), 'data-updated': String(updated), 'data-search': search } });
}

const ann = userBlock('ann', 3, 10, 300, 'ann portfolio');
const bob = userBlock('bob', 1, 90, 100, 'bob roadmap');
const cid = userBlock('cid', 9, 50, 200, 'cid atlas');
const blocks = [ann, bob, cid];

// Creation order and update order deliberately disagree, so a test that passes
// cannot be one that simply left the rows alone.
const rowFresh = siteRow('fresh', 100, 300, 'bob roadmap');
const rowStale = siteRow('stale', 200, 100, 'ann portfolio');
const rowNew = siteRow('new', 300, 200, 'cid atlas');
// The server renders newest-created first, matching the default sort.
const rows = [rowNew, rowStale, rowFresh];

function container(children, selector) {
  return el({
    order: children.slice(),
    querySelectorAll(sel) {
      assert.equal(sel, selector);
      return children;
    },
    appendChild(node) {
      const at = this.order.indexOf(node);
      if (at !== -1) this.order.splice(at, 1);
      this.order.push(node);
    },
  });
}

const userlist = container(blocks, '.user-block');
const sitelist = container(rows, '.site');
userlist.hidden = true;

const filter = el({ value: '' });
const sort = el({
  value: 'recent-sites',
  choose(value) { this.value = value; this.listeners.change(); },
});

global.document = {
  getElementById(id) {
    return { filter, sort, userlist, sitelist }[id];
  },
  querySelectorAll(selector) {
    assert.equal(selector, 'time[data-local-time]');
    return [];
  },
};

new Function(process.env.ADMIN_LIST_SCRIPT)();

const names = () => userlist.order.map((b) => b.username);
const siteNames = () => sitelist.order.map((r) => r.name);

// The flat, newest-created list is what loads. The script must settle this at
// load rather than only on change, or the page disagrees with its own dropdown.
assert.equal(sitelist.hidden, false, 'recently created sites is the default view');
assert.equal(userlist.hidden, true);
assert.deepEqual(siteNames(), ['new', 'stale', 'fresh']);
assert.equal(sitelist.classList.contains('show-updated'), false, 'created view dates rows by creation');

// The other flat sort reorders the same rows rather than showing a second list.
sort.choose('latest-sites');
assert.equal(sitelist.hidden, false);
assert.equal(userlist.hidden, true);
assert.deepEqual(siteNames(), ['fresh', 'new', 'stale']);
assert.equal(sitelist.classList.contains('show-updated'), true, 'updated view dates rows by update');

// And back again.
sort.choose('recent-sites');
assert.deepEqual(siteNames(), ['new', 'stale', 'fresh']);
assert.equal(sitelist.classList.contains('show-updated'), false);

// Ordinary sorts still reorder user blocks, and reveal the grouped list.
sort.choose('sitecount');
assert.equal(userlist.hidden, false);
assert.equal(sitelist.hidden, true, 'user sorts must not reveal the flat list');
assert.deepEqual(names(), ['cid', 'ann', 'bob']);
sort.choose('views');
assert.deepEqual(names(), ['bob', 'cid', 'ann']);
sort.choose('latest');
assert.deepEqual(names(), ['ann', 'cid', 'bob']);
assert.equal(sitelist.hidden, true);

// The filter applies to whichever list is showing.
filter.value = 'roadmap';
filter.listeners.input();
assert.equal(rowFresh.style.display, '');
assert.equal(rowStale.style.display, 'none');
assert.equal(bob.style.display, '');
assert.equal(ann.style.display, 'none');

// Switching back restores the grouped list with the filter still applied.
sort.choose('username');
assert.equal(userlist.hidden, false);
assert.equal(sitelist.hidden, true);
assert.deepEqual(names(), ['ann', 'bob', 'cid']);
assert.equal(bob.style.display, '');
assert.equal(ann.style.display, 'none');
