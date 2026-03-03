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

  function fetchWithTimeout(url, opts, ms) {
    var c = new AbortController();
    var id = setTimeout(function () { c.abort(); }, ms);
    opts = opts || {};
    opts.signal = c.signal;
    return fetch(url, opts).finally(function () { clearTimeout(id); });
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
      var nameSpan = document.createElement('span');
      nameSpan.className = 'tree-name';
      nameSpan.textContent = displayName;
      var metaSpan = document.createElement('span');
      metaSpan.className = 'tree-meta';
      metaSpan.textContent = entry.fileCount + ' files \u00B7 ' + formatBytes(entry.totalSize);
      var delSpan = document.createElement('span');
      delSpan.className = 'tree-action tree-delete';
      delSpan.title = 'Delete';
      delSpan.textContent = '\u2715';
      summary.appendChild(nameSpan);
      summary.appendChild(metaSpan);
      summary.appendChild(delSpan);
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
    var fNameSpan = document.createElement('span');
    fNameSpan.className = 'tree-name';
    fNameSpan.textContent = entry.name.split('/').pop();
    var fMetaSpan = document.createElement('span');
    fMetaSpan.className = 'tree-meta';
    fMetaSpan.textContent = entry.backend + ' \u00B7 ' + formatBytes(entry.totalSize) + ' \u00B7 ' + entry.createdAt;
    var fDelSpan = document.createElement('span');
    fDelSpan.className = 'tree-action tree-delete';
    fDelSpan.title = 'Delete';
    fDelSpan.textContent = '\u2715';
    div.appendChild(fNameSpan);
    div.appendChild(fMetaSpan);
    div.appendChild(fDelSpan);
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

    fetchWithTimeout(url, null, 10000)
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
        if (loading) loading.textContent = err.name === 'AbortError' ? 'Request timed out' : 'Failed to load';
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
  var pendingDeleteIsDir = false;

  if (dialog && tree) {
    tree.addEventListener('click', function (e) {
      var btn = e.target.closest('.tree-delete');
      if (!btn) return;

      var fileEl = btn.closest('.tree-file');
      var dirEl = btn.closest('.tree-dir');

      if (fileEl && fileEl.dataset.key) {
        pendingDeleteKey = fileEl.dataset.key;
        pendingDeleteEl = fileEl;
        pendingDeleteIsDir = false;
        deleteNameEl.textContent = pendingDeleteKey.split('/').pop();
      } else if (dirEl && dirEl.dataset.prefix) {
        e.preventDefault();
        pendingDeleteKey = dirEl.dataset.prefix;
        pendingDeleteEl = dirEl;
        pendingDeleteIsDir = true;
        var metaEl = dirEl.querySelector('summary .tree-meta');
        var metaText = metaEl ? ' (' + metaEl.textContent.trim() + ')' : '';
        deleteNameEl.textContent = pendingDeleteKey + metaText;
      } else {
        return;
      }

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

      var endpoint = pendingDeleteIsDir ? 'api/delete-prefix' : 'api/delete';
      var payload = pendingDeleteIsDir
        ? JSON.stringify({ prefix: pendingDeleteKey })
        : JSON.stringify({ key: pendingDeleteKey });

      fetchWithTimeout(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: payload
      }, 30000)
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
        .catch(function (err) {
          deleteOkBtn.textContent = err.name === 'AbortError' ? 'Request timed out' : 'Network error';
        });
    });
  }

  // --- Upload flow ---
  var uploadBtn = document.getElementById('upload-btn');
  var uploadFilesInput = document.getElementById('upload-files-input');
  var uploadFolderInput = document.getElementById('upload-folder-input');
  var uploadDialog = document.getElementById('upload-dialog');
  var uploadBucketSelect = document.getElementById('upload-bucket');
  var uploadPathInput = document.getElementById('upload-path');
  var uploadAddFilesBtn = document.getElementById('upload-add-files');
  var uploadAddFolderBtn = document.getElementById('upload-add-folder');
  var uploadFileList = document.getElementById('upload-file-list');
  var uploadProgressEl = document.getElementById('upload-progress');
  var uploadCancelBtn = document.getElementById('upload-cancel');
  var uploadOkBtn = document.getElementById('upload-ok');

  var pendingFiles = [];

  function renderFileList() {
    uploadFileList.replaceChildren();
    for (var i = 0; i < pendingFiles.length; i++) {
      var row = document.createElement('div');
      row.className = 'upload-file-row';
      var nameSpan = document.createElement('span');
      nameSpan.textContent = pendingFiles[i].displayName;
      var sizeSpan = document.createElement('span');
      sizeSpan.className = 'upload-file-size';
      sizeSpan.textContent = formatBytes(pendingFiles[i].file.size);
      row.appendChild(nameSpan);
      row.appendChild(sizeSpan);
      uploadFileList.appendChild(row);
    }
    uploadOkBtn.disabled = pendingFiles.length === 0;
  }

  function buildKey(displayName) {
    var bucket = uploadBucketSelect.value;
    var path = uploadPathInput.value.replace(/^\/+|\/+$/g, '');
    if (path) {
      return bucket + '/' + path + '/' + displayName;
    }
    return bucket + '/' + displayName;
  }

  function setUploadControlsDisabled(disabled) {
    uploadAddFilesBtn.disabled = disabled;
    uploadAddFolderBtn.disabled = disabled;
    uploadBucketSelect.disabled = disabled;
    uploadPathInput.disabled = disabled;
    uploadCancelBtn.disabled = disabled;
    uploadOkBtn.disabled = disabled;
  }

  function uploadNext(index, successCount, failCount) {
    if (index >= pendingFiles.length) {
      if (failCount === 0) {
        uploadDialog.close();
        location.reload();
      } else {
        uploadProgressEl.textContent = successCount + ' uploaded, ' + failCount + ' failed';
        uploadCancelBtn.disabled = false;
      }
      return;
    }

    var entry = pendingFiles[index];
    var key = buildKey(entry.displayName);
    uploadProgressEl.hidden = false;
    uploadProgressEl.textContent = 'Uploading ' + (index + 1) + ' of ' + pendingFiles.length + ': ' + entry.displayName;

    var formData = new FormData();
    formData.append('key', key);
    formData.append('file', entry.file);

    fetchWithTimeout('api/upload', { method: 'POST', body: formData }, 120000)
      .then(function (resp) {
        if (resp.status === 401) { location.href = 'login'; return; }
        return resp.json();
      })
      .then(function (data) {
        if (!data) return;
        if (data.ok) {
          uploadNext(index + 1, successCount + 1, failCount);
        } else {
          var rows = uploadFileList.querySelectorAll('.upload-file-row');
          if (rows[index]) rows[index].classList.add('upload-failed');
          uploadNext(index + 1, successCount, failCount + 1);
        }
      })
      .catch(function () {
        var rows = uploadFileList.querySelectorAll('.upload-file-row');
        if (rows[index]) rows[index].classList.add('upload-failed');
        uploadNext(index + 1, successCount, failCount + 1);
      });
  }

  if (uploadBtn && uploadDialog) {
    uploadBtn.addEventListener('click', function () {
      pendingFiles = [];
      uploadFileList.replaceChildren();
      uploadProgressEl.hidden = true;
      uploadProgressEl.textContent = '';
      uploadPathInput.value = '';
      uploadOkBtn.disabled = true;
      uploadOkBtn.textContent = 'Upload';
      setUploadControlsDisabled(false);
      uploadOkBtn.disabled = true;
      uploadDialog.showModal();
    });

    uploadAddFilesBtn.addEventListener('click', function () {
      uploadFilesInput.value = '';
      uploadFilesInput.click();
    });

    uploadFilesInput.addEventListener('change', function () {
      var files = uploadFilesInput.files;
      for (var i = 0; i < files.length; i++) {
        pendingFiles.push({ file: files[i], displayName: files[i].name });
      }
      renderFileList();
    });

    uploadAddFolderBtn.addEventListener('click', function () {
      uploadFolderInput.value = '';
      uploadFolderInput.click();
    });

    uploadFolderInput.addEventListener('change', function () {
      var files = uploadFolderInput.files;
      for (var i = 0; i < files.length; i++) {
        pendingFiles.push({ file: files[i], displayName: files[i].webkitRelativePath });
      }
      renderFileList();
    });

    uploadCancelBtn.addEventListener('click', function () {
      uploadDialog.close();
    });

    uploadOkBtn.addEventListener('click', function () {
      if (pendingFiles.length === 0) return;
      setUploadControlsDisabled(true);
      uploadNext(0, 0, 0);
    });
  }

  // --- Refresh button ---
  var refreshBtn = document.getElementById('refresh-btn');
  if (refreshBtn) {
    refreshBtn.addEventListener('click', function () { location.reload(); });
  }

  // --- Rebalance flow ---
  var rebalanceBtn = document.getElementById('rebalance-btn');

  if (rebalanceBtn) {
    rebalanceBtn.addEventListener('click', function () {
      rebalanceBtn.disabled = true;
      rebalanceBtn.textContent = 'Rebalancing\u2026';

      fetchWithTimeout('api/rebalance', { method: 'POST' }, 60000)
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
        .catch(function (err) {
          rebalanceBtn.textContent = err.name === 'AbortError' ? 'Request timed out' : 'Network error';
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

      fetchWithTimeout('api/sync', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ backend: backend, bucket: bucket })
      }, 60000)
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
        .catch(function (err) {
          syncOkBtn.textContent = err.name === 'AbortError' ? 'Request timed out' : 'Network error';
          setTimeout(function () {
            syncOkBtn.disabled = false;
            syncOkBtn.textContent = 'Sync';
          }, 3000);
        });
    });
  }
})();
