// -------------------------------------------------------------------------------
// Tree - Lazy-loaded directory expansion for the object browser
//
// Author: Alex Freidah
//
// Intercepts <details> open events on directory nodes and fetches children from
// the /ui/api/tree endpoint. Renders the response into the same HTML structure
// as server-rendered nodes. Supports pagination via "load more" links.
// -------------------------------------------------------------------------------

(function () {
  'use strict';

  var tree = document.getElementById('object-tree');
  if (!tree) return;

  tree.addEventListener('toggle', function (e) {
    var details = e.target;
    if (!details.open || details.dataset.loaded === 'true') return;
    if (!details.classList.contains('tree-dir')) return;

    details.dataset.loaded = 'true';
    loadChildren(details.dataset.prefix, '', details.querySelector('.tree-children'));
  }, true);

  function loadChildren(prefix, startAfter, container) {
    var url = 'api/tree?prefix=' + encodeURIComponent(prefix);
    if (startAfter) url += '&startAfter=' + encodeURIComponent(startAfter);

    fetch(url)
      .then(function (resp) {
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        return resp.json();
      })
      .then(function (data) {
        var loading = container.querySelector('.tree-loading');
        if (loading) loading.remove();

        var existing = container.querySelector('.tree-load-more');
        if (existing) existing.remove();

        var entries = data.entries || [];
        for (var i = 0; i < entries.length; i++) {
          container.appendChild(renderEntry(entries[i]));
        }

        if (data.hasMore && data.nextCursor) {
          var btn = document.createElement('div');
          btn.className = 'tree-load-more';
          btn.textContent = 'Load more\u2026';
          btn.addEventListener('click', function () {
            btn.textContent = 'Loading\u2026';
            loadChildren(prefix, data.nextCursor, container);
          });
          container.appendChild(btn);
        }
      })
      .catch(function (err) {
        var loading = container.querySelector('.tree-loading');
        if (loading) loading.textContent = 'Failed to load';
        console.error('Tree load error:', err);
      });
  }

  function renderEntry(entry) {
    if (entry.isDir) {
      var details = document.createElement('details');
      details.className = 'tree-dir';
      details.dataset.prefix = entry.name;
      details.dataset.loaded = 'false';

      var summary = document.createElement('summary');
      var parts = entry.name.replace(/\/$/, '').split('/');
      var displayName = parts[parts.length - 1] + '/';
      summary.innerHTML =
        '<span class="tree-name">' + escapeHtml(displayName) + '</span>' +
        '<span class="tree-meta">' + entry.fileCount + ' files &middot; ' + formatBytes(entry.totalSize) + '</span>';
      details.appendChild(summary);

      var children = document.createElement('div');
      children.className = 'tree-children';
      var loadingDiv = document.createElement('div');
      loadingDiv.className = 'tree-loading';
      loadingDiv.textContent = 'Loading\u2026';
      children.appendChild(loadingDiv);
      details.appendChild(children);

      return details;
    }

    var div = document.createElement('div');
    div.className = 'tree-file';
    div.innerHTML =
      '<span class="tree-name">' + escapeHtml(entry.name.split('/').pop()) + '</span>' +
      '<span class="tree-meta">' + escapeHtml(entry.backend) + ' &middot; ' +
      formatBytes(entry.totalSize) + ' &middot; ' + escapeHtml(entry.createdAt) + '</span>';
    return div;
  }

  function formatBytes(b) {
    if (b === 0) return '0 B';
    var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    var i = 0;
    while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
    return (i === 0 ? b : b.toFixed(1)) + ' ' + units[i];
  }

  function escapeHtml(s) {
    if (!s) return '';
    var d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
  }
})();
