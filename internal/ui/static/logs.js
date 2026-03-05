// -------------------------------------------------------------------------------
// Logs - Fetches and renders log entries from the ring buffer API
//
// Author: Alex Freidah
//
// Fetches /ui/api/logs with level filtering, renders entries with color-coded
// severity, and provides client-side text search. Optional auto-refresh polls
// every 3 seconds when the toggle is enabled. Supports server-side pagination
// via "Load more" button using the `before` parameter.
// -------------------------------------------------------------------------------

(function () {
  'use strict';

  var container = document.getElementById('logs-container');
  var levelSelect = document.getElementById('logs-level');
  var refreshBtn = document.getElementById('logs-refresh');
  var searchInput = document.getElementById('logs-search');
  var autoToggle = document.getElementById('logs-auto');

  if (!container) return;

  var allEntries = [];
  var hasMore = false;
  var oldestTimestamp = '';
  var autoInterval = null;
  var defaultLimit = 100;

  function fetchWithTimeout(url, opts, ms) {
    var c = new AbortController();
    var id = setTimeout(function () { c.abort(); }, ms);
    opts = opts || {};
    opts.signal = c.signal;
    return fetch(url, opts).finally(function () { clearTimeout(id); });
  }

  function levelClass(level) {
    switch (level) {
      case 'ERROR': return 'log-level-error';
      case 'WARN':  return 'log-level-warn';
      case 'INFO':  return 'log-level-info';
      case 'DEBUG': return 'log-level-debug';
      default:      return 'log-level-debug';
    }
  }

  function formatTime(iso) {
    try {
      var d = new Date(iso);
      return d.toLocaleTimeString(undefined, { hour12: false });
    } catch (e) {
      return iso;
    }
  }

  function formatAttrs(attrs) {
    if (!attrs) return '';
    var parts = [];
    for (var key in attrs) {
      if (Object.prototype.hasOwnProperty.call(attrs, key)) {
        parts.push(key + '=' + String(attrs[key]));
      }
    }
    return parts.join(' ');
  }

  function renderEntries(entries) {
    container.style.minHeight = container.offsetHeight + 'px';
    container.replaceChildren();

    if (entries.length === 0) {
      var empty = document.createElement('div');
      empty.className = 'logs-empty';
      empty.textContent = 'No log entries.';
      container.appendChild(empty);
      container.style.minHeight = '';
      return;
    }

    for (var i = entries.length - 1; i >= 0; i--) {
      var e = entries[i];
      var row = document.createElement('div');
      row.className = 'log-entry ' + levelClass(e.level);

      var timeSpan = document.createElement('span');
      timeSpan.className = 'log-time';
      timeSpan.textContent = formatTime(e.time);
      row.appendChild(timeSpan);

      var lvlSpan = document.createElement('span');
      lvlSpan.className = 'log-lvl';
      lvlSpan.textContent = e.level;
      row.appendChild(lvlSpan);

      var msgSpan = document.createElement('span');
      msgSpan.className = 'log-msg';
      msgSpan.textContent = e.message;
      row.appendChild(msgSpan);

      var attrStr = formatAttrs(e.attrs);
      if (attrStr) {
        var attrSpan = document.createElement('span');
        attrSpan.className = 'log-attrs';
        attrSpan.textContent = attrStr;
        row.appendChild(attrSpan);
      }

      container.appendChild(row);
    }

    if (hasMore) {
      var btn = document.createElement('button');
      btn.className = 'btn-action logs-load-more';
      btn.textContent = 'Load more\u2026';
      btn.addEventListener('click', function () {
        btn.disabled = true;
        btn.textContent = 'Loading\u2026';
        loadMore();
      });
      container.appendChild(btn);
    }
    container.style.minHeight = '';
  }

  function applySearch() {
    var scrollY = window.scrollY;
    var term = (searchInput.value || '').toLowerCase();
    if (!term) {
      renderEntries(allEntries);
      window.scrollTo(0, scrollY);
      return;
    }
    var filtered = allEntries.filter(function (e) {
      if (e.message.toLowerCase().indexOf(term) !== -1) return true;
      if (e.level.toLowerCase().indexOf(term) !== -1) return true;
      if (e.attrs) {
        for (var key in e.attrs) {
          if (Object.prototype.hasOwnProperty.call(e.attrs, key)) {
            if (String(e.attrs[key]).toLowerCase().indexOf(term) !== -1) return true;
            if (key.toLowerCase().indexOf(term) !== -1) return true;
          }
        }
      }
      return false;
    });
    renderEntries(filtered);
    window.scrollTo(0, scrollY);
  }

  function buildURL(before) {
    var level = levelSelect.value;
    var url = 'api/logs?limit=' + defaultLimit;
    if (level) url += '&level=' + encodeURIComponent(level);
    if (before) url += '&before=' + encodeURIComponent(before);
    return url;
  }

  function fetchLogs() {
    var scrollY = window.scrollY;
    allEntries = [];
    hasMore = false;
    oldestTimestamp = '';

    var loadingDiv = document.createElement('div');
    loadingDiv.className = 'tree-loading';
    loadingDiv.textContent = 'Loading\u2026';
    container.replaceChildren(loadingDiv);
    window.scrollTo(0, scrollY);

    fetchWithTimeout(buildURL(''), null, 10000)
      .then(function (resp) {
        if (resp.status === 401) { location.href = 'login'; return; }
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        return resp.json();
      })
      .then(function (data) {
        if (!data) return;
        allEntries = data.entries || [];
        hasMore = data.hasMore || false;
        if (allEntries.length > 0) {
          oldestTimestamp = allEntries[0].time;
        }
        applySearch();
      })
      .catch(function (err) {
        container.replaceChildren();
        var errDiv = document.createElement('div');
        errDiv.className = 'logs-empty';
        errDiv.textContent = err.name === 'AbortError'
          ? 'Request timed out'
          : 'Failed to load logs: ' + err.message;
        container.appendChild(errDiv);
        window.scrollTo(0, scrollY);
      });
  }

  function loadMore() {
    if (!oldestTimestamp) return;

    fetchWithTimeout(buildURL(oldestTimestamp), null, 10000)
      .then(function (resp) {
        if (resp.status === 401) { location.href = 'login'; return; }
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        return resp.json();
      })
      .then(function (data) {
        if (!data) return;
        var newEntries = data.entries || [];
        hasMore = data.hasMore || false;
        if (newEntries.length > 0) {
          allEntries = newEntries.concat(allEntries);
          oldestTimestamp = newEntries[0].time;
        }
        applySearch();
      })
      .catch(function (err) {
        var btn = container.querySelector('.logs-load-more');
        if (btn) {
          btn.disabled = false;
          btn.textContent = err.name === 'AbortError' ? 'Request timed out' : 'Failed to load';
        }
      });
  }

  function setAutoRefresh(enabled) {
    if (enabled) {
      if (!autoInterval) autoInterval = setInterval(fetchLogs, 3000);
    } else {
      if (autoInterval) { clearInterval(autoInterval); autoInterval = null; }
    }
  }

  // Event listeners
  refreshBtn.addEventListener('click', fetchLogs);
  levelSelect.addEventListener('change', fetchLogs);
  searchInput.addEventListener('input', applySearch);
  if (autoToggle) {
    autoToggle.addEventListener('change', function () {
      setAutoRefresh(autoToggle.checked);
    });
  }

  // Initial load
  fetchLogs();
})();
