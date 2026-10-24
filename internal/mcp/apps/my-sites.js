(function (root) {
  'use strict';
  // All site and entry strings go through this one text-only constructor.
  function node(document, tag, className, text) {
    const el = document.createElement(tag);
    if (className) el.className = className;
    if (text !== undefined) el.textContent = String(text);
    return el;
  }
  function relative(value, now = Date.now()) {
    const stamp = Date.parse(value);
    if (!Number.isFinite(stamp)) return 'time unavailable';
    const minutes = Math.max(0, Math.floor((now - stamp) / 60000));
    if (minutes < 1) return 'just now';
    if (minutes < 60) return minutes + (minutes === 1 ? ' minute ago' : ' minutes ago');
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return hours + (hours === 1 ? ' hour ago' : ' hours ago');
    const days = Math.floor(hours / 24);
    return days === 1 ? 'yesterday' : days + ' days ago';
  }
  function badge(site) {
    return {only_me:'Owner or team',specific:'Named viewers',company:'Company',listed:'Company showcase',network:'Network'}[site.access] || 'Access unknown';
  }
  const entryPreset = policy => ['inbox', 'records'].includes(policy?.preset) || ['inbox', 'records'].includes(policy?.base);
  function renderEntry(document, entry) {
    const row = node(document, 'div', 'inquiry');
    row.append(node(document, 'strong', '', entry.name || entry.full_name || entry.email || 'Entry'));
    const summary = entry.message || entry.summary || entry.details || entry.subject || entry.title || entry.description ||
      Object.entries(entry).filter(([key, value]) => !['id','visitor_id','created_at','updated_at','name','email','resource'].includes(key) && ['string','number'].includes(typeof value)).map(([, value]) => value).join(' · ');
    row.append(node(document, 'p', '', summary || 'New entry'));
    row.append(node(document, 'time', '', relative(entry.created_at)));
    return row;
  }
  root.SHSites = {node, relative, badge, renderEntry};
  if (typeof document === 'undefined') return;
  const $ = id => document.getElementById(id), app = root.SHApp;
  let sites = [], selected = null, generation = 0, scoped = false, listLoaded = false;
  function renderSites() {
    $('rows').replaceChildren();
    const words = $('search').value.toLowerCase().trim().split(/\s+/);
    const filtered = sites.filter(site => words.every(word => [site.name, site.summary?.title, site.summary?.description, site.url].join(' ').toLowerCase().includes(word)));
    for (const site of filtered) {
      const row = node(document, 'button', 'site-row'); row.type = 'button';
      const tile = node(document, 'span', 'site-thumb', (site.summary?.title || site.name || '?').slice(0, 1).toUpperCase());
      tile.setAttribute('aria-hidden', 'true');
      const info = node(document, 'span', 'row-info');
      info.append(node(document, 'strong', '', site.summary?.title || site.name));
      info.append(node(document, 'span', 'row-description', site.summary?.description || site.summary?.text || site.name));
      const meta = node(document, 'span', 'row-meta');
      meta.append(node(document, 'span', '', 'updated ' + relative(site.updated_at)));
      meta.append(node(document, 'span', 'badge', badge(site))); info.append(meta);
      const chevron = node(document, 'span', 'arrow', '›'); chevron.setAttribute('aria-hidden', 'true');
      row.append(tile, info, chevron);
      row.addEventListener('click', () => {
        if (site.access_role !== 'viewer' && document.body.dataset.dashboard === 'true' && app.hostCapabilities?.serverTools) showDashboard(site);
        else app.openLink(site.url).catch(() => { $('list-status').textContent = 'Could not open this site. Try again.'; });
      });
      $('rows').append(row);
    }
    $('view-label').textContent = scoped ? 'Showing ' + sites.length + ' sites' : 'My sites';
    $('list-status').textContent = filtered.length ? '' : sites.length ? 'No matching sites.' : 'No sites yet.';
  }
  async function loadAnalytics(site, current) {
    try {
      const result = await app.callTool('site_activity', {owner:site.owner_username, site:site.name, days:7});
      if (current !== generation) return;
      const visits = result.visits;
      if (!visits) throw new Error(result.visits_note || 'Visitor counts unavailable');
      $('visitor-count').textContent = Number.isFinite(visits.unique_viewers) ? visits.unique_viewers.toLocaleString() : 'Unavailable';
      const daily = Array.from({length:7}, (_, i) => {
        const date = new Date(visits.from); date.setUTCDate(date.getUTCDate()+i);
        const day = date.toISOString().slice(0,10);
        const row = visits.days.find(row => row.day.slice(0,10) === day);
        return {day, visitors:row?.unique_viewers || 0};
      });
      const max = Math.max(1, ...daily.map(day => day.visitors || 0));
      const labels = [];
      for (const day of daily) {
        const count = day.visitors || 0;
        const label = new Date(day.day + 'T00:00:00Z').toLocaleDateString(undefined, {weekday: 'short', timeZone: 'UTC'});
        labels.push(day.day + ': ' + count + ' visitors');
        const column = node(document, 'div', 'chart-column'); column.title = labels.at(-1);
        const bar = node(document, 'span', 'chart-bar'); bar.style.height = (count / max * 110) + 'px';
        column.append(node(document, 'span', 'chart-count', count), bar, node(document, 'span', 'chart-day', label)); $('chart').append(column);
      }
      $('chart').setAttribute('aria-label', labels.join(', '));
    } catch (_) { if (current === generation) { $('visitor-count').textContent = 'Unavailable'; $('analytics-error').textContent = 'Could not load visitors. Access may be restricted by this instance.'; } }
  }
  async function loadEntries(site, current) {
    try {
      const listing = await app.callTool('storage_list_resources', {owner:site.owner_username, site:site.name});
      const entries = [], sources = [], failed = [];
      for (const resource of listing.resources || []) {
        if (current !== generation) return;
        // Key-value namespaces and file buckets are not table entries.
        if (resource.kind !== 'db') continue;
        for (const [name, policy] of Object.entries(resource.tables || {})) {
          if (!entryPreset(policy)) continue;
          if (current !== generation) return;
          const source = resource.name + ' / ' + name;
          try {
            const result = await app.callTool('storage_list_rows', {owner:site.owner_username,site:site.name,resource:resource.name,table:name,order:'created_at',desc:true,limit:3});
            sources.push(source); entries.push(...result.rows.map(row => ({...row, resource:source})));
          } catch { failed.push(source); }
        }
      }
      if (current !== generation) return;
      if (failed.length) $('entries-error').textContent = 'Could not load: ' + failed.join(', ') + '.';
      if (!sources.length) return;
      entries.sort((a, b) => (Date.parse(b.created_at) || 0) - (Date.parse(a.created_at) || 0) || Number(b.id) - Number(a.id));
      $('entries').hidden = false;
      $('entries-title').textContent = 'Latest entries · ' + sources.join(', ');
      for (const entry of entries.slice(0, 3)) $('entry-rows').append(renderEntry(document, entry));
      if (!entries.length) $('entry-rows').append(node(document, 'p', 'subtle', 'No entries yet.'));
    } catch (_) { if (current === generation) $('entries-error').textContent = 'Could not load entries. Reopen this view to try again.'; }
  }
  function showDashboard(site) {
    selected = site; const current = ++generation;
    $('site-list').hidden = true; $('dashboard').hidden = false; $('view-label').textContent = 'Weekly overview';
    $('site-name').textContent = site.summary?.title || site.name; $('visitor-count').textContent = '…';
    for (const id of ['chart','entry-rows','analytics-error','entries-error','link-error']) $(id).replaceChildren();
    $('entries').hidden = true; $('back').focus();
    loadAnalytics(site, current); loadEntries(site, current);
  }
  $('search').addEventListener('input', renderSites);
  $('back').addEventListener('click', () => {
    ++generation; selected = null; $('dashboard').hidden = true; $('site-list').hidden = false;
    $('view-label').textContent = 'My sites'; renderSites(); $('search').focus();
  });
  $('open-site').addEventListener('click', () => app.openLink(selected.url).catch(() => { $('link-error').textContent = 'Could not open this site. Try again.'; }));
  app.on('tool-input', params => {
    scoped = ['include'].some(key => Object.hasOwn(params.arguments || {}, key));
  });
  function listFailed(message) {
    if (listLoaded) return;
    sites = []; $('rows').replaceChildren();
    $('list-status').textContent = message;
  }
  app.on('tool-cancelled', () => listFailed('Loading sites was cancelled. Use the chat to try again.'));
  app.on('tool-result', result => {
    if (result.isError) { listFailed('Could not load sites. Use the chat to try again.'); return; }
    if (!Array.isArray(result.structuredContent?.items)) return;
    listLoaded = true;
    sites = result.structuredContent.items;
    if (!selected) renderSites();
  });
  app.start().catch(() => { $('list-status').textContent = 'Could not connect. Use the site links in the chat.'; });
})(globalThis);
