package handler

// dashboardVersionsScript is the Versions list in a site's Manage panel:
// every kept version with who deployed it and when, a Preview link that
// opens it (preview.go) in a new tab, and Make live, which is the rollback to
// it. It defines window.shSiteVersions(list, base), which the
// sites script calls with the panel's list element and the site's
// owner-qualified API path; it is kept apart so the sites script only mounts
// it.
const dashboardVersionsScript = `<script>
(function(){
  var CH = {'X-Simple-Host-Client': 'control-ui'};
  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML.replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }

  window.shSiteVersions = function(list, base) {
    var etag = '';
    function load() {
      fetch(base + '/versions', {credentials: 'same-origin', headers: CH})
        .then(function(r){ etag = r.headers.get('ETag') || ''; return r.json(); })
        .then(function(versions){
          list.innerHTML = '';
          if (!versions || !versions.length) { list.innerHTML = '<div class="rank-empty">No versions yet.</div>'; return; }
          versions.forEach(function(v){
            var row = document.createElement('div');
            row.className = 'rank-row';
            var who = v.uploaded_by ? ' by ' + esc(v.uploaded_by) : '';
            row.innerHTML = '<span class="rank-name">Version ' + esc(v.version_number) +
              ' <span class="rank-sub">' + (v.live ? 'live · ' : '') + esc(new Date(v.created_at).toLocaleString()) + who + '</span></span>' +
              '<button type="button" class="btn-reject preview-version" data-version="' + esc(v.version_number) + '">Preview</button>' +
              (v.live ? '' : ' <button type="button" class="btn-login make-live" data-version="' + esc(v.version_number) + '">Make live</button>');
            list.appendChild(row);
          });
        })
        .catch(function(){ list.innerHTML = '<div class="rank-empty">Could not load versions.</div>'; });
    }

    list.addEventListener('click', function(ev){
      var preview = ev.target.closest('.preview-version');
      if (preview) {
        // Opened before the request so the browser treats it as the click's
        // own window, then pointed at the link once it arrives.
        var win = window.open('', '_blank');
        fetch(base + '/versions/' + encodeURIComponent(preview.getAttribute('data-version')) + '/preview', {credentials: 'same-origin', headers: CH})
          .then(function(r){ return r.json().then(function(b){
            if (!r.ok) { if (win) win.close(); alert('Could not open the preview: ' + (b.error || 'unknown error')); return; }
            if (win) { win.opener = null; win.location = b.url; } else { location.href = b.url; }
          }); })
          .catch(function(){ if (win) win.close(); alert('Network error opening the preview.'); });
        return;
      }
      var live = ev.target.closest('.make-live');
      if (!live) return;
      var version = Number(live.getAttribute('data-version'));
      if (!confirm('Make version ' + version + ' live? Visitors see it at once. The version live now stays kept and can be made live again.')) return;
      fetch(base + '/rollback', {
        method: 'POST', credentials: 'same-origin',
        headers: Object.assign({'Content-Type': 'application/json', 'If-Match': etag}, CH),
        body: JSON.stringify({version: version}),
      }).then(function(r){
        if (!r.ok) return r.json().then(function(b){ alert('Could not make it live: ' + (b.error || 'unknown error')); load(); });
        load();
      }).catch(function(){ alert('Network error making the version live.'); });
    });

    load();
  };
})();
</script>`
