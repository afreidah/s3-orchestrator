// -------------------------------------------------------------------------------
// Tree - Lazy-loaded directory expansion + file management for the object browser
//
// Author: Alex Freidah
//
// Intercepts <details> open events on directory nodes and fetches children from
// the /ui/api/tree endpoint. Renders the response into the same HTML structure
// as server-rendered nodes. Supports pagination via "load more" links.
// Also handles delete confirmation and upload dialogs.
// -------------------------------------------------------------------------------

(function () {
  'use strict';

  // --- Helpers ---
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
    div.dataset.key = entry.name;
    div.innerHTML =
      '<span class="tree-name">' + escapeHtml(entry.name.split('/').pop()) + '</span>' +
      '<span class="tree-meta">' + escapeHtml(entry.backend) + ' &middot; ' +
      formatBytes(entry.totalSize) + ' &middot; ' + escapeHtml(entry.createdAt) + '</span>' +
      '<span class="tree-action tree-delete" title="Delete">&#x2715;</span>';
    return div;
  }

  // --- Lazy-loaded tree expansion ---
  var tree = document.getElementById('object-tree');

  if (tree) {
    tree.addEventListener('toggle', function (e) {
      var details = e.target;
      if (!details.open || details.dataset.loaded === 'true') return;
      if (!details.classList.contains('tree-dir')) return;

      details.dataset.loaded = 'true';
      loadChildren(details.dataset.prefix, '', details.querySelector('.tree-children'));
    }, true);
  }

  function loadChildren(prefix, startAfter, container) {
    var url = 'api/tree?prefix=' + encodeURIComponent(prefix);
    if (startAfter) url += '&startAfter=' + encodeURIComponent(startAfter);

    fetch(url)
      .then(function (resp) {
        if (resp.status === 401) { location.href = 'login'; return; }
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        return resp.json();
      })
      .then(function (data) {
        if (!data) return;
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

  // --- Delete confirmation flow ---
  var dialog = document.getElementById('confirm-delete');
  var deleteNameEl = document.getElementById('confirm-delete-name');
  var deleteCancelBtn = document.getElementById('confirm-delete-cancel');
  var deleteOkBtn = document.getElementById('confirm-delete-ok');
  var pendingDeleteKey = '';
  var pendingDeleteEl = null;

  if (dialog && tree) {
    tree.addEventListener('click', function (e) {
      var btn = e.target.closest('.tree-delete');
      if (!btn) return;

      var fileEl = btn.closest('.tree-file');
      if (!fileEl || !fileEl.dataset.key) return;

      pendingDeleteKey = fileEl.dataset.key;
      pendingDeleteEl = fileEl;
      deleteNameEl.textContent = pendingDeleteKey.split('/').pop();
      deleteOkBtn.disabled = false;
      deleteOkBtn.textContent = 'Delete';
      dialog.showModal();
    });

    deleteCancelBtn.addEventListener('click', function () {
      dialog.close();
    });

    deleteOkBtn.addEventListener('click', function () {
      deleteOkBtn.disabled = true;
      deleteOkBtn.textContent = 'Deleting\u2026';

      fetch('api/delete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key: pendingDeleteKey })
      })
        .then(function (resp) {
          if (resp.status === 401) { location.href = 'login'; return; }
          return resp.json();
        })
        .then(function (data) {
          if (!data) return;
          if (data.ok) {
            dialog.close();
            location.reload();
          } else {
            deleteOkBtn.textContent = data.error || 'Failed';
          }
        })
        .catch(function () {
          deleteOkBtn.textContent = 'Network error';
        });
    });
  }

  // --- Upload flow ---
  var uploadBtn = document.getElementById('upload-btn');
  var uploadFileInput = document.getElementById('upload-file');
  var uploadDialog = document.getElementById('upload-dialog');
  var uploadKeyInput = document.getElementById('upload-key');
  var uploadCancelBtn = document.getElementById('upload-cancel');
  var uploadOkBtn = document.getElementById('upload-ok');
  var uploadStatus = document.getElementById('upload-status');
  var buckets = window.__buckets || [];
  var defaultPrefix = buckets.length ? buckets[0] + '/' : '';

  if (uploadBtn && uploadDialog) {
    // Step 1: Click Upload button → open native file picker
    uploadBtn.addEventListener('click', function () {
      uploadFileInput.value = '';
      uploadFileInput.click();
    });

    // Step 2: File selected → show dialog with pre-filled path
    uploadFileInput.addEventListener('change', function () {
      if (!uploadFileInput.files.length) return;
      var filename = uploadFileInput.files[0].name;
      uploadKeyInput.value = defaultPrefix + filename;
      uploadOkBtn.disabled = false;
      uploadOkBtn.textContent = 'Upload';
      uploadDialog.showModal();
    });

    uploadCancelBtn.addEventListener('click', function () {
      uploadDialog.close();
    });

    // Step 3: Confirm → upload
    uploadOkBtn.addEventListener('click', function () {
      if (!uploadKeyInput.value || !uploadFileInput.files.length) return;

      uploadOkBtn.disabled = true;
      uploadOkBtn.textContent = 'Uploading\u2026';

      var formData = new FormData();
      formData.append('key', uploadKeyInput.value);
      formData.append('file', uploadFileInput.files[0]);

      fetch('api/upload', { method: 'POST', body: formData })
        .then(function (resp) {
          if (resp.status === 401) { location.href = 'login'; return; }
          return resp.json();
        })
        .then(function (data) {
          if (!data) return;
          if (data.ok) {
            uploadDialog.close();
            location.reload();
          } else {
            uploadOkBtn.textContent = data.error || 'Failed';
            setTimeout(function () {
              uploadOkBtn.disabled = false;
              uploadOkBtn.textContent = 'Upload';
            }, 3000);
          }
        })
        .catch(function () {
          uploadOkBtn.textContent = 'Network error';
          setTimeout(function () {
            uploadOkBtn.disabled = false;
            uploadOkBtn.textContent = 'Upload';
          }, 3000);
        });
    });
  }

  // --- Rebalance flow ---
  var rebalanceBtn = document.getElementById('rebalance-btn');

  if (rebalanceBtn) {
    rebalanceBtn.addEventListener('click', function () {
      rebalanceBtn.disabled = true;
      rebalanceBtn.textContent = 'Rebalancing\u2026';

      fetch('api/rebalance', { method: 'POST' })
        .then(function (resp) {
          if (resp.status === 401) { location.href = 'login'; return; }
          return resp.json();
        })
        .then(function (data) {
          if (!data) return;
          if (data.ok) {
            rebalanceBtn.textContent = data.moved + ' moved';
            setTimeout(function () { location.reload(); }, 1500);
          } else {
            rebalanceBtn.textContent = data.error || 'Failed';
            setTimeout(function () {
              rebalanceBtn.disabled = false;
              rebalanceBtn.textContent = 'Rebalance';
            }, 3000);
          }
        })
        .catch(function () {
          rebalanceBtn.textContent = 'Network error';
          setTimeout(function () {
            rebalanceBtn.disabled = false;
            rebalanceBtn.textContent = 'Rebalance';
          }, 3000);
        });
    });
  }

  // --- Sync flow ---
  var syncBtn = document.getElementById('sync-btn');
  var syncDialog = document.getElementById('sync-dialog');
  var syncCancelBtn = document.getElementById('sync-cancel');
  var syncOkBtn = document.getElementById('sync-ok');

  if (syncBtn && syncDialog) {
    syncBtn.addEventListener('click', function () {
      syncOkBtn.disabled = false;
      syncOkBtn.textContent = 'Sync';
      syncDialog.showModal();
    });

    syncCancelBtn.addEventListener('click', function () {
      syncDialog.close();
    });

    syncOkBtn.addEventListener('click', function () {
      var backend = document.getElementById('sync-backend').value;
      var bucket = document.getElementById('sync-bucket').value;
      if (!backend || !bucket) return;

      syncOkBtn.disabled = true;
      syncOkBtn.textContent = 'Syncing\u2026';

      fetch('api/sync', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ backend: backend, bucket: bucket })
      })
        .then(function (resp) {
          if (resp.status === 401) { location.href = 'login'; return; }
          return resp.json();
        })
        .then(function (data) {
          if (!data) return;
          if (data.ok) {
            syncOkBtn.textContent = data.imported + ' imported';
            setTimeout(function () {
              syncDialog.close();
              location.reload();
            }, 1500);
          } else {
            syncOkBtn.textContent = data.error || 'Failed';
            setTimeout(function () {
              syncOkBtn.disabled = false;
              syncOkBtn.textContent = 'Sync';
            }, 3000);
          }
        })
        .catch(function () {
          syncOkBtn.textContent = 'Network error';
          setTimeout(function () {
            syncOkBtn.disabled = false;
            syncOkBtn.textContent = 'Sync';
          }, 3000);
        });
    });
  }
})();
