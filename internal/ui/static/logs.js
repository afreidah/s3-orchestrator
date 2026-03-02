// -------------------------------------------------------------------------------
// Logs - Fetches and renders log entries from the ring buffer API
//
// Author: Alex Freidah
//
// Fetches /ui/api/logs with level filtering, renders entries with color-coded
// severity, and provides client-side text search. Optional auto-refresh polls
// every 3 seconds when the toggle is enabled.
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
  var autoInterval = null;

  function escapeHtml(s) {
    if (!s) return '';
    var d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
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
        parts.push(escapeHtml(key) + '=' + escapeHtml(String(attrs[key])));
      }
    }
    return parts.join(' ');
  }

  function renderEntries(entries) {
    if (entries.length === 0) {
      container.innerHTML = '<div class="logs-empty">No log entries.</div>';
      return;
    }

    var html = '';
    for (var i = entries.length - 1; i >= 0; i--) {
      var e = entries[i];
      var attrStr = formatAttrs(e.attrs);
      html += '<div class="log-entry ' + levelClass(e.level) + '">' +
        '<span class="log-time">' + escapeHtml(formatTime(e.time)) + '</span>' +
        '<span class="log-lvl">' + escapeHtml(e.level) + '</span>' +
        '<span class="log-msg">' + escapeHtml(e.message) + '</span>' +
        (attrStr ? '<span class="log-attrs">' + attrStr + '</span>' : '') +
        '</div>';
    }
    container.innerHTML = html;
  }

  function applySearch() {
    var term = (searchInput.value || '').toLowerCase();
    if (!term) {
      renderEntries(allEntries);
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
  }

  function fetchLogs() {
    var level = levelSelect.value;
    var url = 'api/logs?limit=500';
    if (level) url += '&level=' + encodeURIComponent(level);

    container.innerHTML = '<div class="tree-loading">Loading\u2026</div>';

    fetch(url)
      .then(function (resp) {
        if (resp.status === 401) { location.href = 'login'; return; }
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        return resp.json();
      })
      .then(function (data) {
        if (!data) return;
        allEntries = data;
        applySearch();
      })
      .catch(function (err) {
        container.innerHTML = '<div class="logs-empty">Failed to load logs: ' +
          escapeHtml(err.message) + '</div>';
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
