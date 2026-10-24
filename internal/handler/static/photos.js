(function () {
  'use strict';
  const $ = id => document.getElementById(id);
  const status = text => { $('status').textContent = text; };
  const shrink = file => SHPhotos.shrink(file, { maxBytes: Number($('photo-page').dataset.maxBytes), maxDimension: Number($('photo-page').dataset.maxDimension) });
  const selected = new URLSearchParams(location.search).get('site') || '';
  const account = new URLSearchParams(location.search).get('account') || '';
  let busy = false, photos = [];
  const slug = SHPhotos.slug;
  const size = bytes => bytes >= 1000000 ? (bytes / 1000000).toFixed(1) + ' MB' : Math.max(1, Math.round(bytes / 1000)) + ' KB';
  function element(tag, text, parent) {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (parent) parent.appendChild(node);
    return node;
  }
  async function request(path, options = {}) {
    const response = await fetch(path, { ...options, credentials: 'same-origin' });
    const body = await response.json();
    if (!response.ok) throw Object.assign(new Error(body.error || 'Could not save. Try again.'), { status: response.status, body });
    return body;
  }
  function lock(on) {
    busy = on;
    $('files').disabled = on;
    $('site').disabled = on;
    $('save').disabled = on || !photos.length || !$('site').value;
    $('photos').querySelectorAll('button,input').forEach(node => { node.disabled = on; });
  }
  async function loadSites() {
    const [me, sites] = await Promise.all([request('/api/me'), request('/api/collaboration/sites')]);
    $('site').replaceChildren();
    for (const site of sites.filter(s => ['owner','member'].includes(s.access_role))) {
      const name = site.owner_username + '/' + site.name;
      const option = element('option', name, $('site')); option.value = name;
    }
    if (account && account !== me.username) {
      $('site').selectedIndex = -1;
      status('This link is for ' + account + '; you are signed in as ' + me.username + '. Choose a site you can edit.');
    } else if (selected) {
      const match = Array.from($('site').options).filter(o => o.value === selected || o.value === me.username + '/' + selected);
      $('site').value = match.length === 1 ? match[0].value : '';
      if (!match.length) status('Choose a site you can edit.');
    }
    $('editor').hidden = !$('site').options.length;
    if ($('editor').hidden) status('You have no sites you can edit.');
    lock(false);
  }
  function showPhoto(photo, original) {
    const row = element('div', '', $('photos')); row.className = 'photo';
    const img = element('img', '', row); img.alt = 'Photo preview';
    photo.url = URL.createObjectURL(photo.blob); img.src = photo.url;
    const details = element('div', '', row);
    const label = element('label', 'Photo name', details);
    const input = element('input', '', label); input.value = slug(original.name); input.maxLength = 60; input.setAttribute('aria-describedby', 'name-hint');
    element('small', original.name + ': ' + size(original.size) + ' → ' + size(photo.blob.size) + ' (' + photo.ext.slice(1) + ')', details);
    photo.input = input;
    const remove = element('button', 'Remove', details); remove.type = 'button'; remove.className = 'btn btn-secondary';
    remove.addEventListener('click', () => { URL.revokeObjectURL(photo.url); photos = photos.filter(item => item !== photo); row.remove(); lock(false); });
  }
  async function choose(files) {
    if (busy) return;
    if (files.length + photos.length > 30) { status('Choose up to 30 photos at a time.'); return; }
    lock(true);
    const failures = [];
    for (const file of files) {
      status('Preparing ' + file.name + '…');
      try { const photo = await shrink(file); photos.push(photo); showPhoto(photo, file); lock(true); }
      catch (error) { failures.push(file.name + ': ' + error.message); }
    }
    $('files').value = '';
    lock(false);
    status(failures.length ? failures.join(' ') : photos.length + ' photos ready to save.');
  }
  async function copy(text, button) {
    try { await navigator.clipboard.writeText(text); button.textContent = 'Copied'; }
    catch (_) {
      const box = element('textarea', text, button.parentNode); box.readOnly = true; box.focus(); box.select();
      status('Select and copy the text shown here.');
    }
  }
  $('files').addEventListener('change', event => choose(Array.from(event.target.files)));
  $('site').addEventListener('change', () => {
    history.replaceState({}, '', '/photos?site=' + encodeURIComponent($('site').value)); lock(false);
  });
  for (const name of ['dragenter', 'dragover']) $('drop').addEventListener(name, event => { event.preventDefault(); $('drop').classList.add('drag'); });
  for (const name of ['dragleave', 'drop']) $('drop').addEventListener(name, event => { event.preventDefault(); $('drop').classList.remove('drag'); });
  $('drop').addEventListener('drop', event => choose(Array.from(event.dataTransfer.files)));
  $('save').addEventListener('click', async () => {
    if (busy || !photos.length || !$('site').value) return;
    const data = new FormData();
    for (const photo of photos) {
      const name = slug(photo.input.value); photo.input.value = name;
      data.append('photos', photo.blob, name + photo.ext);
    }
    lock(true); status('Saving photos…');
    try {
      const [owner, site] = $('site').value.split('/');
      const result = await request('/v1/sites/' + encodeURIComponent(site) + '/photos?owner=' + encodeURIComponent(owner), { method: 'POST', body: data });
      $('results').replaceChildren();
      for (const photo of result.photos) {
        const row = element('div', '', $('results')); row.className = 'result';
        const link = element('a', photo.url, row); link.href = photo.url;
        const button = element('button', 'Copy address', row); button.className = 'btn btn-secondary';
        button.addEventListener('click', () => copy(photo.url, button));
      }
      $('summary').textContent = 'I added ' + result.photos.map(photo => photo.path).join(' and ') + ' to the site ' + result.site + '.';
      $('editor').hidden = true; $('done').hidden = false;
      status('Saved as version ' + result.version + '.');
      photos.forEach(photo => URL.revokeObjectURL(photo.url)); photos = [];
    } catch (error) { status(error.message); }
    finally { lock(false); }
  });
  $('again').addEventListener('click', event => {
    event.preventDefault();
    $('photos').replaceChildren(); $('results').replaceChildren();
    $('done').hidden = true; $('editor').hidden = false;
    status('Choose more photos.'); lock(false);
  });
  $('copy-summary').addEventListener('click', () => copy($('summary').textContent, $('copy-summary')));
  window.addEventListener('pagehide', () => photos.forEach(photo => URL.revokeObjectURL(photo.url)));
  loadSites().catch(error => status(error.message));
})();
