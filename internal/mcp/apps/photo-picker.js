(function () {
  'use strict';
  function occupiedPaths(files) {
    const occupied = new Map();
    for (const file of files) {
      occupied.set(file.path, false);
      const parts = file.path.split('/');
      for (let i = 1; i < parts.length; i++) occupied.set(parts.slice(0, i).join('/'), true);
    }
    return occupied;
  }
  function photoPath(occupied, name) {
    if (occupied.get('images') === false) throw new Error('The images path is a file. Rename it before adding photos.');
    const dot = name.lastIndexOf('.'), stem = name.slice(0, dot), ext = name.slice(dot);
    let path = 'images/' + name;
    for (let n = 2; occupied.has(path); n++) {
      if (occupied.get(path)) throw new Error('A folder uses this photo name. Choose another name.');
      const suffix = '-' + n;
      path = 'images/' + stem.slice(0, 60 - suffix.length) + suffix + ext;
    }
    occupied.set(path, false);
    return path;
  }
  // A selection is one update and therefore one site version.
  function batches(photos, site, maxBodyBytes) {
    const files = photos.map(photo => ({path:photo.path,content:photo.base64,encoding:'base64'}));
    const bytes = new TextEncoder().encode(JSON.stringify({jsonrpc:'2.0',id:1,method:'tools/call',params:{name:'deploy_site',arguments:{site,owner:site,etag:'reserved',intent:'update',keep_other_files:true,files}}})).length + 4096;
    if (bytes > maxBodyBytes) throw new Error('Choose fewer photos to save them in one version.');
    return photos.length ? [photos] : [];
  }
  globalThis.SHPhotoBatch = { occupiedPaths, photoPath, batches };
  if (typeof document === 'undefined') return;
  const $ = id => document.getElementById(id), app = SHApp, config = SHPhotoConfig;
  let site = '', busy = false, ready = false, photos = [], saved = 0, finished = false, archive = false;
  const status = text => { $('status').textContent = text; };
  function lock() {
    $('add').disabled = busy || !ready || !site;
    $('add').hidden = finished || archive || busy || photos.filter(photo => !photo.failed).length >= 30;
    for (const photo of photos) { photo.remove.hidden = !!photo.saved; photo.remove.disabled = busy; }
    $('change').disabled = busy || !ready || saved > 0;
    $('sites').disabled = busy || saved > 0;
    $('finish').disabled = busy || !site || !(photos.some(photo => photo.blob && !photo.saved) || (saved > 0 && photos.every(photo => photo.saved)));
  }
  function selectSite(name) {
    if (site !== name) for (const photo of photos) { delete photo.path; delete photo.attempted; }
    site = name; $('site').textContent = name; lock();
  }
  function element(tag, text, parent, className) {
    const node = document.createElement(tag); node.textContent = text; if (className) node.className = className;
    parent.appendChild(node); return node;
  }
  function workerFactory() {
    const base = config.assetOrigin + '/photo-codecs/v2/';
    // Cross-origin Worker(url) is forbidden. Blob workers inherit the host CSP;
    // importScripts uses resourceDomains, including script-wrapped Wasm bytes.
    const source = 'self.photoCodecBase=' + JSON.stringify(base) + ';importScripts(' + JSON.stringify(base + 'decoder.js?apps=1') + ');';
    const url = URL.createObjectURL(new Blob([source], { type: 'text/javascript' }));
    try {
      const worker = new Worker(url);
      const terminate = worker.terminate.bind(worker);
      worker.terminate = () => { terminate(); URL.revokeObjectURL(url); };
      return worker;
    } catch (error) { URL.revokeObjectURL(url); throw error; }
  }
  // Only the site/error state is consumed. Never parse, retain, or use photos_url.
  app.on('tool-input', params => {
    if (params.arguments?.archive === true) { archive = true; ready = false; document.body.hidden = true; return; }
    if (archive || finished || busy) return;
    if (typeof params.arguments?.site === 'string') selectSite(params.arguments.site);
  });
  app.on('tool-result', params => {
    if (archive || finished || busy) return;
    if (typeof params.structuredContent?.site === 'string') selectSite(params.structuredContent.site);
    // A host without interactive tool calls keeps the tool's existing chat text.
    if (!app.hostCapabilities?.serverTools) { document.body.hidden = true; ready = false; return; }
    ready = !params.isError;
    if (!ready) status('Could not save. Try again.');
    lock();
  });
  app.on('tool-cancelled', () => { ready = false; lock(); });
  $('change').addEventListener('click', async () => {
    busy = true; lock();
    try {
      const result = await app.callTool('list_sites');
      $('sites').replaceChildren();
      for (const item of (result.items || []).filter(item => ['owner','member'].includes(item.access_role))) {
        const name = item.owner_username + '/' + item.name;
        const option = element('option', name, $('sites')); option.value = name;
      }
      $('sites').value = site; $('sites').hidden = false; $('sites').focus();
    } catch (error) { status(error.message); }
    finally { busy = false; lock(); }
  });
  $('sites').addEventListener('change', () => { selectSite($('sites').value); $('sites').hidden = true; });
  $('add').addEventListener('click', () => $('files').click());
  $('files').addEventListener('change', async event => {
    const files = Array.from(event.target.files); event.target.value = '';
    if (busy || !files.length) return;
    if (files.length + photos.filter(photo => !photo.failed).length > 30) { status('Choose up to 30 photos at a time.'); return; }
    busy = true; lock(); $('add').hidden = true; $('actions').hidden = false;
    for (const [index, file] of files.entries()) {
      status('Shrinking photos... ' + (index + 1) + ' of ' + files.length);
      const row = element('div', '', $('photos'));
      const image = element('img', '', row, 'photo-visual'); image.alt = 'Photo preview'; image.hidden = true;
      element('div', file.name, row, 'photo-name');
      const progress = element('progress', '', row, 'progress'); progress.max = 100; progress.setAttribute('aria-label', file.name);
      const label = element('div', 'Shrinking', row, 'progress-label');
      const remove = element('button', 'Remove', row, 'text-button'); remove.type = 'button'; remove.disabled = true;
      remove.setAttribute('aria-label', 'Remove ' + file.name);
      const photo = { name: file.name, label, progress, remove }; photos.push(photo);
      remove.addEventListener('click', () => {
        if (busy || photo.saved) return;
        photos = photos.filter(item => item !== photo); row.remove();
        if (saved > 0 && photos.every(item => item.saved)) status('');
        $('fallback').hidden = !photos.some(item => item.failed); lock();
      });
      try {
        Object.assign(photo, await SHPhotos.shrink(file, { ...config, maxBytes: Math.min(config.maxBytes || 1000000, Math.floor((config.maxInlineBytes || 8388608)/30)), workerFactory }));
        image.onerror = () => { image.hidden = true; };
        image.src = await SHPhotos.dataURL(photo.blob); image.hidden = false;
        progress.value = 100; label.textContent = 'Shrunk · ready';
      } catch (_) {
        photo.failed = true; delete photo.blob;
        progress.value = 0; label.textContent = 'Could not read this photo';
        $('fallback').hidden = false;
      }
    }
    busy = false; lock();
  });
  function markSaved(photo) {
    if (!photo.saved) { photo.saved = true; saved++; }
    photo.label.textContent = 'Added'; photo.progress.value = 100;
  }
  function complete() {
    finished = true; status('');
    for (const id of ['selection', 'sites', 'status', 'add', 'photos', 'actions', 'fallback']) $(id).hidden = true;
    $('done').textContent = 'Added ' + saved + (saved === 1 ? ' photo' : ' photos') + ' to images/'; $('done').hidden = false;
    photos = [];
  }
  $('finish').addEventListener('click', async () => {
    if (busy || !site) return;
    if (saved > 0 && photos.every(photo => photo.saved)) { complete(); lock(); return; }
    busy = true; lock(); status('Saving photos...');
    let remaining = photos.filter(photo => photo.blob && !photo.saved);
    try {
      const [owner, name] = site.split('/');
      if (!owner || !name) throw new Error('Choose a site from the list.');
      const target = {owner,site:name};
      const current = await app.callTool('get_site', target);
      const listing = await app.callTool('list_site_files', {...target,version:current.active_version});
      current.files = listing.files;
      if (!Array.isArray(current.files)) throw new Error('Could not read the site files. Try again.');
      // Reconcile a lost reply before assigning any new names. Retry the same
      // reserved path when the previous write cannot be confirmed.
      for (const photo of remaining) {
        if (photo.attempted && current.files.some(file => file.path === photo.path)) {
          const stored = await app.callTool('read_site_file', {...target,version:current.active_version,path:photo.path});
          if (stored.encoding === 'base64' && stored.content === photo.base64) markSaved(photo);
          else throw new Error('A file changed during the save. Remove this photo and choose it again.');
        }
      }
      remaining = remaining.filter(photo => !photo.saved);
      const occupied = occupiedPaths(current.files);
      for (const photo of remaining) if (photo.path) occupied.set(photo.path, false);
      for (const photo of remaining) {
        photo.path ||= photoPath(occupied, SHPhotos.slug(photo.name) + photo.ext);
        photo.base64 ||= (await SHPhotos.dataURL(photo.blob)).split(',')[1];
      }
      for (const batch of batches(remaining, site, config.maxBodyBytes)) {
        const files = [];
        for (const photo of batch) { files.push({path:photo.path,content:photo.base64,encoding:'base64'}); photo.attempted = true; photo.label.textContent = 'Saving'; photo.progress.removeAttribute('value'); }
        try {
          await app.callTool('deploy_site', { ...target, etag:current.etag, intent:'update', keep_other_files:true, files });
          for (const photo of batch) markSaved(photo);
        } catch (error) {
          for (const photo of batch) { photo.label.textContent = error.message; photo.progress.value = 0; }
          throw error;
        }
      }
      complete();
    } catch (error) { status(error.message); }
    finally { busy = false; lock(); }
  });
  app.start().catch(error => status(error.message));
})();
