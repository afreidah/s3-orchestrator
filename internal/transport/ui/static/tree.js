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
    let units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let i = 0;
    while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
    return (i === 0 ? b : b.toFixed(1)) + ' ' + units[i];
  }

  function getCSRFToken() {
    let match = document.cookie.match('(?:^|; )s3orch_csrf=([^;]*)');
    return match ? match[1] : '';
  }

  function fetchWithTimeout(url, opts, ms) {
    let c = new AbortController();
    let id = setTimeout(function () { c.abort(); }, ms);
    opts = opts || {};
    opts.signal = c.signal;
    // Include CSRF token on state-changing requests
    if (opts.method === 'POST') {
      opts.headers = opts.headers || {};
      opts.headers['X-CSRF-Token'] = getCSRFToken();
    }
    return fetch(url, opts).finally(function () { clearTimeout(id); });
  }

  function renderEntry(entry) {
    if (entry.isDir) {
      let details = document.createElement('details');
      details.className = 'tree-dir';
      details.dataset.prefix = entry.name;
      details.dataset.loaded = 'false';

      let summary = document.createElement('summary');
      let parts = entry.name.replace(/\/$/, '').split('/');
      let displayName = parts[parts.length - 1] + '/';
      let nameSpan = document.createElement('span');
      nameSpan.className = 'tree-name';
      nameSpan.textContent = displayName;
      let metaSpan = document.createElement('span');
      metaSpan.className = 'tree-meta';
      metaSpan.textContent = entry.fileCount + ' files \u00B7 ' + formatBytes(entry.totalSize);
      let delSpan = document.createElement('span');
      delSpan.className = 'tree-action tree-delete';
      delSpan.title = 'Delete';
      delSpan.textContent = '\u2715';
      summary.appendChild(nameSpan);
      summary.appendChild(metaSpan);
      summary.appendChild(delSpan);
      details.appendChild(summary);

      let children = document.createElement('div');
      children.className = 'tree-children';
      let loadingDiv = document.createElement('div');
      loadingDiv.className = 'tree-loading';
      loadingDiv.textContent = 'Loading\u2026';
      children.appendChild(loadingDiv);
      details.appendChild(children);

      return details;
    }

    let div = document.createElement('div');
    div.className = 'tree-file';
    div.dataset.key = entry.name;
    let fNameSpan = document.createElement('span');
    fNameSpan.className = 'tree-name';
    fNameSpan.textContent = entry.name.split('/').pop();
    let fMetaSpan = document.createElement('span');
    fMetaSpan.className = 'tree-meta';
    let backendsLabel = Array.isArray(entry.backends) ? entry.backends.join(', ') : '';
    fMetaSpan.textContent = backendsLabel + ' \u00B7 ' + formatBytes(entry.totalSize) + ' \u00B7 ' + entry.createdAt;
    let fDlSpan = document.createElement('span');
    fDlSpan.className = 'tree-action tree-download';
    fDlSpan.title = 'Download';
    fDlSpan.textContent = '\u2193';
    let fDelSpan = document.createElement('span');
    fDelSpan.className = 'tree-action tree-delete';
    fDelSpan.title = 'Delete';
    fDelSpan.textContent = '\u2715';
    div.appendChild(fNameSpan);
    div.appendChild(fMetaSpan);
    div.appendChild(fDlSpan);
    div.appendChild(fDelSpan);
    return div;
  }

  // --- Lazy-loaded tree expansion ---
  let tree = document.getElementById('object-tree');

  if (tree) {
    tree.addEventListener('toggle', function (e) {
      let details = e.target;
      if (!details.open || details.dataset.loaded === 'true') return;
      if (!details.classList.contains('tree-dir')) return;

      details.dataset.loaded = 'true';
      loadChildren(details.dataset.prefix, '', details.querySelector('.tree-children'));
    }, true);
  }

  function loadChildren(prefix, startAfter, container) {
    let url = 'api/tree?prefix=' + encodeURIComponent(prefix);
    if (startAfter) url += '&startAfter=' + encodeURIComponent(startAfter);

    fetchWithTimeout(url, null, 10000)
      .then(function (resp) {
        if (resp.status === 401) { location.href = 'login'; return; }
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        return resp.json();
      })
      .then(function (data) {
        if (!data) return;
        let loading = container.querySelector('.tree-loading');
        if (loading) loading.remove();

        let existing = container.querySelector('.tree-load-more');
        if (existing) existing.remove();

        let entries = data.entries || [];
        for (let i = 0; i < entries.length; i++) {
          container.appendChild(renderEntry(entries[i]));
        }

        if (data.hasMore && data.nextCursor) {
          let btn = document.createElement('div');
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
        let loading = container.querySelector('.tree-loading');
        if (loading) loading.textContent = err.name === 'AbortError' ? 'Request timed out' : 'Failed to load';
        console.error('Tree load error:', err);
      });
  }

  // --- Delete confirmation flow ---
  let dialog = document.getElementById('confirm-delete');
  let deleteNameEl = document.getElementById('confirm-delete-name');
  let deleteCancelBtn = document.getElementById('confirm-delete-cancel');
  let deleteOkBtn = document.getElementById('confirm-delete-ok');
  let pendingDeleteKey = '';
  let pendingDeleteEl = null;
  let pendingDeleteIsDir = false;

  if (dialog && tree) {
    tree.addEventListener('click', function (e) {
      let btn = e.target.closest('.tree-delete');
      if (!btn) return;

      let fileEl = btn.closest('.tree-file');
      let dirEl = btn.closest('.tree-dir');

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
        let metaEl = dirEl.querySelector('summary .tree-meta');
        let metaText = metaEl ? ' (' + metaEl.textContent.trim() + ')' : '';
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

      let endpoint = pendingDeleteIsDir ? 'api/delete-prefix' : 'api/delete';
      let payload = pendingDeleteIsDir
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

  // --- Download ---
  if (tree) {
    tree.addEventListener('click', function (e) {
      let btn = e.target.closest('.tree-download');
      if (!btn) return;
      let fileEl = btn.closest('.tree-file');
      if (!fileEl || !fileEl.dataset.key) return;
      window.location = 'api/download?key=' + encodeURIComponent(fileEl.dataset.key);
    });
  }

  // --- Upload flow ---
  let uploadBtn = document.getElementById('upload-btn');
  let uploadFilesInput = document.getElementById('upload-files-input');
  let uploadFolderInput = document.getElementById('upload-folder-input');
  let uploadDialog = document.getElementById('upload-dialog');
  let uploadBucketSelect = document.getElementById('upload-bucket');
  let uploadPathInput = document.getElementById('upload-path');
  let uploadAddFilesBtn = document.getElementById('upload-add-files');
  let uploadAddFolderBtn = document.getElementById('upload-add-folder');
  let uploadFileList = document.getElementById('upload-file-list');
  let uploadProgressEl = document.getElementById('upload-progress');
  let uploadCancelBtn = document.getElementById('upload-cancel');
  let uploadOkBtn = document.getElementById('upload-ok');

  let pendingFiles = [];

  function renderFileList() {
    uploadFileList.replaceChildren();
    for (let i = 0; i < pendingFiles.length; i++) {
      let row = document.createElement('div');
      row.className = 'upload-file-row';
      let nameSpan = document.createElement('span');
      nameSpan.textContent = pendingFiles[i].displayName;
      let sizeSpan = document.createElement('span');
      sizeSpan.className = 'upload-file-size';
      sizeSpan.textContent = formatBytes(pendingFiles[i].file.size);
      row.appendChild(nameSpan);
      row.appendChild(sizeSpan);
      uploadFileList.appendChild(row);
    }
    uploadOkBtn.disabled = pendingFiles.length === 0;
  }

  function buildKey(displayName) {
    let bucket = uploadBucketSelect.value;
    let path = uploadPathInput.value.replaceAll(/^\/+|\/+$/g, '');
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

    let entry = pendingFiles[index];
    let key = buildKey(entry.displayName);
    uploadProgressEl.hidden = false;
    uploadProgressEl.textContent = 'Uploading ' + (index + 1) + ' of ' + pendingFiles.length + ': ' + entry.displayName;

    let formData = new FormData();
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
          let rows = uploadFileList.querySelectorAll('.upload-file-row');
          if (rows[index]) rows[index].classList.add('upload-failed');
          uploadNext(index + 1, successCount, failCount + 1);
        }
      })
      .catch(function () {
        let rows = uploadFileList.querySelectorAll('.upload-file-row');
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
      let files = uploadFilesInput.files;
      for (let i = 0; i < files.length; i++) {
        pendingFiles.push({ file: files[i], displayName: files[i].name });
      }
      renderFileList();
    });

    uploadAddFolderBtn.addEventListener('click', function () {
      uploadFolderInput.value = '';
      uploadFolderInput.click();
    });

    uploadFolderInput.addEventListener('change', function () {
      let files = uploadFolderInput.files;
      for (let i = 0; i < files.length; i++) {
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

  // --- Refresh button (partial update, preserves tree + logs + scroll) ---
  let refreshBtn = document.getElementById('refresh-btn');
  if (refreshBtn) {
    refreshBtn.addEventListener('click', function () {
      refreshBtn.disabled = true;
      refreshBtn.textContent = 'Refreshing\u2026';

      fetchWithTimeout(location.href, null, 10000)
        .then(function (resp) {
          if (resp.status === 401) { location.href = 'login'; return; }
          if (!resp.ok) throw new Error('HTTP ' + resp.status);
          return resp.text();
        })
        .then(function (html) {
          if (!html) return;
          let doc = new DOMParser().parseFromString(html, 'text/html');
          let sections = document.querySelectorAll('.container > section');
          let newSections = doc.querySelectorAll('.container > section');

          // Replace storage summary, backends, and monthly usage (first 3 sections)
          for (let i = 0; i < 3 && i < sections.length && i < newSections.length; i++) {
            sections[i].innerHTML = newSections[i].innerHTML;
          }

          // Update header badges (healthy/degraded)
          let headerRight = document.querySelector('.header-right');
          let newHeaderRight = doc.querySelector('.header-right');
          if (headerRight && newHeaderRight) {
            let badge = headerRight.querySelector('.badge');
            let newBadge = newHeaderRight.querySelector('.badge');
            if (badge && newBadge) {
              badge.className = newBadge.className;
              badge.textContent = newBadge.textContent;
            }
          }

          refreshBtn.disabled = false;
          refreshBtn.textContent = 'Refresh';
        })
        .catch(function () {
          refreshBtn.disabled = false;
          refreshBtn.textContent = 'Refresh';
        });
    });
  }

  // --- Async operation helper: trigger POST, then poll status endpoint ---
  function runAsyncOp(btn, triggerUrl, statusUrl, label, countKey, noun, opts) {
    let options = opts || {};
    btn.disabled = true;
    btn.textContent = label + '\u2026';

    fetchWithTimeout(triggerUrl, { method: 'POST' }, 10000)
      .then(function (resp) {
        if (resp.status === 401) { location.href = 'login'; return; }
        if (resp.status === 409) {
          btn.textContent = 'Already running';
          setTimeout(function () { btn.disabled = false; btn.textContent = label; }, 3000);
          return;
        }
        pollStatus(btn, statusUrl, label, countKey, noun, options);
      })
      .catch(function (err) {
        btn.textContent = err.name === 'AbortError' ? 'Request timed out' : 'Network error';
        setTimeout(function () { btn.disabled = false; btn.textContent = label; }, 3000);
      });
  }

  function pollStatus(btn, statusUrl, label, countKey, noun, options) {
    let suffix = noun ? ' ' + noun : '';
    let poll = setInterval(function () {
      fetch(statusUrl)
        .then(function (resp) { return resp.json(); })
        .then(function (data) {
          if (data.status === 'running') return;
          clearInterval(poll);
          if (data.status === 'skipped') {
            btn.textContent = 'Skipped';
            setStatusBanner(data.reason || 'skipped', 'skipped');
            setTimeout(function () { btn.disabled = false; btn.textContent = label; }, 3000);
            return;
          }
          if (data.ok) {
            let extra = '';
            if (typeof data.failed === 'number' && data.failed > 0) {
              extra = ', ' + data.failed + ' failed';
            }
            btn.textContent = data[countKey] + suffix + extra;
            if (options.skipReload) {
              setTimeout(function () { btn.disabled = false; btn.textContent = label; }, 3000);
            } else {
              setTimeout(function () { location.reload(); }, 1500);
            }
          } else if (data.status === 'error') {
            btn.textContent = data.error || 'Failed';
            setStatusBanner(data.error || 'failed', 'error');
            setTimeout(function () { btn.disabled = false; btn.textContent = label; }, 3000);
          } else {
            btn.disabled = false;
            btn.textContent = label;
          }
        })
        .catch(function () {
          clearInterval(poll);
          btn.textContent = 'Poll error';
          setTimeout(function () { btn.disabled = false; btn.textContent = label; }, 3000);
        });
    }, 2000);
  }

  function setStatusBanner(text, kind) {
    let banner = document.getElementById('admin-action-status');
    if (!banner) return;
    banner.textContent = text;
    banner.className = 'admin-action-status' + (kind ? ' ' + kind : '');
    setTimeout(function () { banner.textContent = ''; banner.className = 'admin-action-status'; }, 6000);
  }

  // --- Rebalance flow ---
  let rebalanceBtn = document.getElementById('rebalance-btn');

  if (rebalanceBtn) {
    rebalanceBtn.addEventListener('click', function () {
      runAsyncOp(rebalanceBtn, 'api/rebalance', 'api/rebalance/status', 'Rebalance', 'moved', 'moved');
    });
  }

  // --- Clean excess flow ---
  let cleanExcessBtn = document.getElementById('clean-excess-btn');

  if (cleanExcessBtn) {
    cleanExcessBtn.addEventListener('click', function () {
      runAsyncOp(cleanExcessBtn, 'api/clean-excess', 'api/clean-excess/status', 'Clean Excess', 'removed', 'removed');
    });
  }

  // --- Replicate Now flow ---
  let replicateBtn = document.getElementById('replicate-btn');

  if (replicateBtn) {
    replicateBtn.addEventListener('click', function () {
      runAsyncOp(replicateBtn, 'api/replicate', 'api/replicate/status', 'Replicate Now', 'copies_created', 'copies created');
    });
  }

  // --- Scrub flow ---
  let scrubBtn = document.getElementById('scrub-btn');

  if (scrubBtn) {
    scrubBtn.addEventListener('click', function () {
      runAsyncOp(scrubBtn, 'api/scrub', 'api/scrub/status', 'Scrub', 'checked', 'checked', { skipReload: true });
    });
  }

  // --- Backfill checksums flow ---
  let backfillBtn = document.getElementById('backfill-checksums-btn');

  if (backfillBtn) {
    backfillBtn.addEventListener('click', function () {
      runAsyncOp(backfillBtn, 'api/backfill-checksums', 'api/backfill-checksums/status', 'Backfill Checksums', 'processed', 'processed', { skipReload: true });
    });
  }

  // --- Encrypt existing flow ---
  let encryptExistingBtn = document.getElementById('encrypt-existing-btn');

  if (encryptExistingBtn) {
    encryptExistingBtn.addEventListener('click', function () {
      if (!confirm('Encrypt every existing unencrypted object? This can take a long time.')) return;
      runAsyncOp(encryptExistingBtn, 'api/encrypt-existing', 'api/encrypt-existing/status', 'Encrypt Existing', 'encrypted', 'encrypted', { skipReload: true });
    });
  }

  // --- Sync flow ---
  let syncBtn = document.getElementById('sync-btn');
  let syncDialog = document.getElementById('sync-dialog');
  let syncCancelBtn = document.getElementById('sync-cancel');
  let syncOkBtn = document.getElementById('sync-ok');

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
      let backend = document.getElementById('sync-backend').value;
      let bucket = document.getElementById('sync-bucket').value;
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
