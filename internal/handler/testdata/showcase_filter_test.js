'use strict';

const assert = require('node:assert/strict');

function eventTarget(properties) {
  return Object.assign({
    listeners: {},
    addEventListener(type, listener) {
      this.listeners[type] = listener;
    },
  }, properties);
}

const form = eventTarget({});
const input = eventTarget({
  value: 'alice',
  focused: false,
  focus() { this.focused = true; },
});
const clear = eventTarget({ hidden: false });
const count = { textContent: '', classList: { toggle() {} } };
const announcer = { textContent: '' };
const sortGo = { hidden: false };
const sortSel = eventTarget({
  value: 'views',
  selectedIndex: 0,
  options: [
    { value: 'views', text: 'Most viewed (7d)' },
    { value: 'updated', text: 'Recently updated' },
    { value: 'newest', text: 'Newest' },
    { value: 'owner', text: 'Owner A-Z' },
    { value: 'name', text: 'Site name A-Z' },
  ],
  choose(value) {
    this.value = value;
    this.selectedIndex = this.options.findIndex((o) => o.value === value);
    this.listeners.change();
  },
});

// Ranks are assigned by the server, one per ordering. Each site gets a
// deliberately different position in each, so an ordering that silently does
// nothing is visible.
function site(name, filterText, ranks) {
  return {
    name,
    dataset: { filterText },
    hidden: false,
    attrs: {
      'data-rank-views': String(ranks.views),
      'data-rank-updated': String(ranks.updated),
      'data-rank-newest': String(ranks.newest),
      'data-rank-owner': String(ranks.owner),
      'data-rank-name': String(ranks.name),
    },
    getAttribute(key) { return this.attrs[key]; },
  };
}

const alice = site('portfolio', 'alice portfolio', { views: 0, updated: 1, newest: 2, owner: 0, name: 1 });
const bob = site('roadmap', 'bob roadmap', { views: 2, updated: 0, newest: 1, owner: 1, name: 2 });
const carol = site('atlas', 'carol atlas', { views: 1, updated: 2, newest: 0, owner: 2, name: 0 });
const allSites = [alice, bob, carol];

const list = {
  order: allSites.slice(),
  appendChild(node) {
    const at = this.order.indexOf(node);
    if (at !== -1) { this.order.splice(at, 1); }
    this.order.push(node);
  },
};

global.document = {
  querySelector(selector) {
    assert.equal(selector, '.filter-form');
    return form;
  },
  getElementById(id) {
    return {
      'showcase-filter': input,
      'showcase-filter-clear': clear,
      'showcase-sites': list,
      'showcase-sort': sortSel,
      'showcase-count': count,
      'showcase-sort-go': sortGo,
      'showcase-announcer': announcer,
    }[id];
  },
  querySelectorAll(selector) {
    if (selector === '.site[data-filter-text]') return allSites;
    throw new Error(`unexpected selector: ${selector}`);
  },
};

new Function(process.env.SHOWCASE_FILTER_SCRIPT)();

const names = () => list.order.map((s) => s.name);

// The no-JS submit button is hidden once the script runs.
assert.equal(sortGo.hidden, true, 'sort submit button should be hidden when JS runs');

// --- filtering ---
assert.equal(alice.hidden, false);
assert.equal(bob.hidden, true);
assert.equal(carol.hidden, true);
assert.equal(clear.hidden, false);
assert.match(count.textContent, /^1 of 3 sites/);

input.value = 'ROADMAP';
input.listeners.input();
assert.equal(alice.hidden, true);
assert.equal(bob.hidden, false);

input.value = 'missing';
let prevented = false;
form.listeners.submit({ preventDefault() { prevented = true; } });
assert.equal(prevented, true);
assert.equal(count.textContent, 'No sites match this filter.');

clear.listeners.click();
assert.equal(input.value, '');
assert.equal(input.focused, true);
assert.deepEqual(allSites.map((s) => s.hidden), [false, false, false]);
assert.equal(clear.hidden, true);
assert.match(count.textContent, /^3 sites/);

// --- sorting follows the server's ranks ---
sortSel.choose('views');
assert.deepEqual(names(), ['portfolio', 'atlas', 'roadmap']);

sortSel.choose('updated');
assert.deepEqual(names(), ['roadmap', 'portfolio', 'atlas']);

sortSel.choose('newest');
assert.deepEqual(names(), ['atlas', 'roadmap', 'portfolio']);

sortSel.choose('owner');
assert.deepEqual(names(), ['portfolio', 'roadmap', 'atlas']);

sortSel.choose('name');
assert.deepEqual(names(), ['atlas', 'portfolio', 'roadmap']);

// An unknown sort falls back to the default ordering rather than doing nothing.
sortSel.value = 'nonsense';
sortSel.selectedIndex = -1;
sortSel.listeners.change();
assert.deepEqual(names(), ['portfolio', 'atlas', 'roadmap']);

// The chosen order is announced in the live region.
sortSel.choose('updated');
assert.match(count.textContent, /sorted by recently updated$/);

// Sorting must not resurrect rows the filter has hidden.
input.value = 'alice';
input.listeners.input();
sortSel.choose('newest');
assert.equal(alice.hidden, false);
assert.equal(bob.hidden, true);
assert.equal(carol.hidden, true);
assert.match(count.textContent, /^1 of 3 sites/);

// The visible tally updates immediately; the live region is debounced so a
// screen reader is not made to narrate every keystroke.
input.value = 'atlas';
input.listeners.input();
assert.match(count.textContent, /^1 of 3 sites/, 'visible count updates synchronously');
assert.notEqual(announcer.textContent, count.textContent, 'announcement is deferred, not immediate');
