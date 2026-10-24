(function () {
  'use strict';
  const app = window.SHApp, $ = id => document.getElementById('preview-' + id);
  let link = '', copyTimer;
  function https(value) {
    if (typeof value !== 'string') return '';
    try { const url = new URL(value); return url.protocol === 'https:' && !url.username && !url.password ? value : ''; }
    catch { return ''; }
  }
  app.on('tool-result', result => {
    const site = result.isError ? null : result.structuredContent;
    if (!site || typeof site.name !== 'string' || typeof site.url !== 'string') return;
    clearTimeout(copyTimer); $('copy').textContent = 'Copy link';
    $('copy-fallback').hidden = true; $('status').textContent = '';
    $('card').hidden = !site;
    if (!site) return;
    const name = site.name;
    link = https(site.url);
    $('title').textContent = name; $('tile').textContent = name;
    $('open').disabled = !link; $('copy').disabled = !link;
    $('tile').hidden = false;
    $('full').hidden = true;
  });
  $('open').onclick = async () => {
    try { await app.openLink(link); }
    catch { $('status').textContent = 'Could not open the link. Copy it to open in your browser.'; }
  };
  $('copy').onclick = async () => {
    try {
      await navigator.clipboard.writeText(link);
      $('copy-fallback').hidden = true;
      $('copy').textContent = 'Copied';
      clearTimeout(copyTimer);
      copyTimer = setTimeout(() => { $('copy').textContent = 'Copy link'; }, 1500);
    } catch {
      $('copy-fallback').hidden = false;
      $('link').value = link;
      $('link').focus(); $('link').select();
    }
  };
  app.start().catch(() => { $('card').hidden = true; });
})();
