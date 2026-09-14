---
description: "Interactive diagram of access control: how a credential resolves to a user, where the registry comes from, and how one grant model authorizes the S3 API, the admin API and the dashboard."
title: "Access Control Flow"
linkTitle: "Access Control Flow"
weight: 11
---

Access control: the one credential type, the user it proves, and the grants that decide what that user reaches on every surface. **Hover over any component** for implementation details.

### How it works

There is one credential: an access key and a secret. It arrives as an AWS SigV4 signature, as presigned query parameters, or typed into the dashboard's login form, and all three resolve the same way. Nothing authorizes by being a particular kind of secret; a credential proves an identity, and that identity's grants answer everything after.

A **grant** pairs a user with a resource and a set of permissions. There are three kinds of resource, and they name three genuinely different things rather than three layers of one:

- **`bucket:photos`** - the virtual bucket clients address. Object access lives here.
- **`backend:wasabi-eu`** - one storage provider the orchestrator writes to. A single object in a bucket may have copies on several backends at once, and a backend grant is about those copies rather than about the objects a client sees. `backend:*` is every provider, including ones added later.
- **`orchestrator`** - the service running in front of both, and what a grant names for work belonging to no single bucket or provider: reading logs, rotating keys, flushing the cache, creating users. It carries no name, because a deployment has one orchestrator.

The permissions are a bitmask - `list`, `read`, `write`, `delete`, `tags`, `list-buckets` on the data plane, and ten `admin-*` permissions on the control plane, in a disjoint bit range. A data-plane set can therefore never be mistaken for an administrative one.

#### Why one registry

The registry is assembled from both places a deployment declares credentials - the config file and the store - and published behind an atomic pointer. A credential issued or revoked through the provisioning API takes effect on the next request rather than the next restart, and the S3 path, the admin API and the dashboard all read the same pointer, so none of them can disagree about who a key belongs to.

That is also what makes the root credential unremarkable. `auth.root` merges in as a user holding every permission on every resource; it reaches everything because of what it holds, not because the request path checks for it.

#### Why verification is constant time

An access key the registry does not hold still computes a full HMAC, against a fixed dummy secret, before the comparison fails. Returning early on an unknown key would make an unknown key measurably faster than a known one with a wrong secret, which is enough to enumerate valid access keys without ever guessing a secret.

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-tooltip {
    position: fixed; z-index: 9999;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4); display: none;
  }
  #ac-tooltip a { color: #34b882; text-decoration: none; }
  #ac-tooltip a:hover { text-decoration: underline; }
  #ac-tooltip h3 { color: #2a9d73; font-size: 0.85rem; margin: 0 0 0.25rem 0; }
  #ac-tooltip .ac-badge {
    display: inline-block; padding: 1px 7px; border-radius: 4px;
    font-size: 0.6rem; font-weight: 600; margin-bottom: 0.4rem; text-transform: uppercase;
  }
  .ac-badge-entry { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-filter { background: #6b5b2e22; color: #c4a35a; border: 1px solid #c4a35a55; }
  .ac-badge-decision { background: #2a9d7322; color: #2a9d73; border: 1px solid #2a9d7355; }
  .ac-badge-process { background: #2d7d6a22; color: #5ec9a0; border: 1px solid #5ec9a055; }
  .ac-badge-storage { background: #1a3a3022; color: #4aaa8a; border: 1px solid #4aaa8a55; }
  .ac-badge-success { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-reject { background: #8b3a3a22; color: #d4a0a0; border: 1px solid #d4a0a055; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #4aaa8a; }
  #ac-tooltip .ac-metric { color: #a7d5c1; font-style: italic; font-size: 0.7rem; }
  #ac-diagram .node, #ac-diagram .edgePath, #ac-diagram .edgeLabel { transition: opacity 0.15s, filter 0.15s; }
  #ac-diagram svg.highlighting .node, #ac-diagram svg.highlighting .edgePath, #ac-diagram svg.highlighting .edgeLabel { opacity: 0.12; }
  #ac-diagram svg.highlighting .node.highlight, #ac-diagram svg.highlighting .edgePath.highlight, #ac-diagram svg.highlighting .edgeLabel.highlight { opacity: 1; filter: drop-shadow(0 0 6px rgba(42,157,115,0.5)); }
  #ac-diagram .node { cursor: pointer; }
</style>

<div id="ac-diagram"></div>
<div id="ac-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11.8.0/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'flowchart TD',
    '    HDR([SigV4 Authorization<br>header]):::entry --> KEY',
    '    QRY([Presigned URL<br>query parameters]):::entry --> KEY',
    '    FORM([Dashboard login<br>access key + secret]):::entry --> KEY',
    '',
    '    KEY[Take the access key]:::process --> LOOKUP',
    '',
    '    CFG[/"config file<br>buckets + auth.root"/]:::entry --> MERGE',
    '    DB[("store<br>users, credentials, grants")]:::storage --> MERGE',
    '    MERGE[Merge into one view,<br>swap behind an atomic pointer]:::process --> REG',
    '    REG[("BucketRegistry<br>access key to user")]:::storage --> LOOKUP',
    '',
    '    LOOKUP[Resolve the credential<br>to its user]:::storage --> VERIFY',
    '    VERIFY{"Secret proves it?<br>constant time"}:::filter -->|no| UNAUTH[401 Unauthorized]:::reject',
    '    VERIFY -->|yes| WHO[Authenticated user<br>and the grants it holds]:::process',
    '',
    '    WHO --> SURFACE{Which surface?}:::decision',
    '    SURFACE -->|dashboard| SESSION[Session cookie<br>carries the user id]:::success',
    '    SURFACE -->|"S3 API, admin objects"| BUCKET[Bucket from the<br>URL path]:::process',
    '    SURFACE -->|admin control plane| RES[Resource the route declares<br>orchestrator, backend:name, backend:*]:::process',
    '',
    '    BUCKET --> REACH{Grant on<br>that bucket?}:::decision',
    '    REACH -->|no| FORBID',
    '    REACH -->|yes| BITS{"Grant carries<br>the operation bit?"}:::filter',
    '    BITS -->|no| FORBID[403 Forbidden]:::reject',
    '    BITS -->|yes| ALLOW',
    '',
    '    RES --> ARES{Grant on<br>that resource?}:::decision',
    '    ARES -->|no| FORBID',
    '    ARES -->|yes| ABITS{"Grant carries<br>the admin-* bit?"}:::filter',
    '    ABITS -->|no| FORBID',
    '    ABITS -->|yes| ALLOW[Request proceeds,<br>audit names the user]:::success',
    '',
    '    classDef entry fill:#1a7a5a,stroke:#1a7a5a,color:#fff,font-weight:bold',
    '    classDef filter fill:#6b5b2e,stroke:#c4a35a,color:#fff',
    '    classDef decision fill:#1e2a26,stroke:#2a9d73,color:#e6edf3,font-size:11px',
    '    classDef process fill:#2d7d6a,stroke:#5ec9a0,color:#fff',
    '    classDef storage fill:#1a3a30,stroke:#4aaa8a,color:#c9d1d9',
    '    classDef success fill:#1a7a5a,stroke:#34b882,color:#fff,font-weight:bold',
    '    classDef reject fill:#8b3a3a,stroke:#d4a0a0,color:#fff,font-weight:bold'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false,
    theme: 'base',
    themeVariables: {
      darkMode: true,
      background: '#191c23',
      fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
      fontSize: '15px',
      primaryColor: '#26332f',
      primaryTextColor: '#f8fafc',
      primaryBorderColor: '#2a9d73',
      secondaryColor: '#3a2e20',
      secondaryTextColor: '#e8dfd0',
      secondaryBorderColor: '#c4a35a',
      tertiaryColor: '#20262d',
      tertiaryTextColor: '#e8dfd0',
      tertiaryBorderColor: '#4aaa8a',
      lineColor: '#7f8b86',
      edgeLabelBackground: '#191c23',
      clusterBkg: '#1d2229',
      clusterBorder: '#39443f'
    },
    flowchart: { nodeSpacing: 32, rankSpacing: 46, curve: 'linear', padding: 12, diagramPadding: 16, useMaxWidth: true, htmlLabels: true }
  });

  mermaid.render('ac-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    HDR: {
      title: 'SigV4 Authorization header',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Standard AWS Signature Version 4. Compatible with the AWS CLI, every AWS SDK, rclone and any S3 client, because nothing about it is specific to this orchestrator.</p><p>The canonical URI is taken from the request\'s wire form, so a key containing <code>%2F</code> - a literal slash in the key rather than a directory separator - round-trips correctly, and an upstream proxy that re-encodes a path after the client signed cannot have the substituted form silently accepted.</p><p>Streaming-payload uploads are accepted and the chunk chain is validated end to end.</p><p><a href="../admission-control/">Admission control &rarr;</a></p>'
    },
    QRY: {
      title: 'Presigned URL query parameters',
      badge: 'entry', badgeText: 'entry point',
      body: '<p><code>X-Amz-Algorithm</code>, <code>X-Amz-Credential</code>, <code>X-Amz-Date</code>, <code>X-Amz-Expires</code>, <code>X-Amz-SignedHeaders</code> and <code>X-Amz-Signature</code> carry the same signature in the URL instead of a header, so a link can be handed to something that cannot sign.</p><p>The same credentials as a normal request, with no additional configuration: a keypair that reaches a bucket can presign for it. Expiry is capped at seven days, and the payload hash is always <code>UNSIGNED-PAYLOAD</code>.</p>'
    },
    FORM: {
      title: 'Dashboard login',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>The login form takes an access key and secret rather than a password of its own. Any credential the deployment holds logs in - the root keypair, or one the provisioning API issued.</p><p>The keypair is presented whole rather than as a signature, so this is the one path that compares the secret directly. Both halves are always compared, so a wrong access key costs the same work as a wrong secret and the response cannot be used to learn which was which.</p>'
    },
    KEY: {
      title: 'Take the access key',
      badge: 'process', badgeText: 'parse',
      body: '<p>The access key is the first field of the credential scope, in the header or in <code>X-Amz-Credential</code>. It names an identity; it proves nothing on its own.</p><p>Access key IDs are globally unique across every bucket and both sources. A key claimed twice has no unambiguous identity, so assembly refuses it and the deployment fails to start rather than resolving to whichever was written last.</p>'
    },
    CFG: {
      title: 'The config file',
      badge: 'entry', badgeText: 'source',
      body: '<p>Two things: the credentials declared per bucket under <code>buckets[].credentials</code>, and the root keypair under <code>auth.root</code>.</p><p>A config-declared credential merges in as a user reaching exactly the one bucket that declared it, so it resolves through the same user-and-grant chain as any other and the request path has no second case to handle.</p><p>Config entries are read-only to the API, which answers <code>403</code> to any attempt to change one: an operator reading the file has to be able to trust what it says. Change those by editing the file and sending <code>SIGHUP</code>.</p><p><a href="../../docs/configuration/#auth">Configuration reference &rarr;</a></p>'
    },
    DB: {
      title: 'The store',
      badge: 'storage', badgeText: 'metadata store',
      body: '<p>Users, their credentials, and their grants as rows, created through the provisioning API or the <code>bucket</code>, <code>user</code>, <code>credential</code> and <code>grant</code> CLI commands.</p><p>A stored credential can reach several buckets, because its user can hold several grants, and a bucket can be granted to several users. That is what the config file has no syntax for.</p><p>The secret is returned once, by the request that minted it, and never read back into any listing. A client that loses it gets a replacement keypair rather than a recovery.</p><p><a href="../database-schema/">Database schema &rarr;</a></p>'
    },
    MERGE: {
      title: 'Merge and publish',
      badge: 'process', badgeText: 'assembly',
      body: '<p>Both sources are merged into one view: buckets, users, credentials and grants, each entry carrying the source it came from. Config wins a collision, and a shadowed stored entry is reported rather than silently dropped.</p><p>The assembled registry is swapped whole behind an atomic pointer. A request either sees the old registry or the new one, never a half-applied one, so a reload cannot leave a window where a revoked credential still works.</p>'
    },
    REG: {
      title: 'BucketRegistry',
      badge: 'storage', badgeText: 'in memory',
      body: '<p>Maps an access key to the credential that holds it and the user behind it, and holds each user\'s grants. Read on every request; rebuilt only when the config reloads or a provisioning call changes a row.</p><p>One registry serves all three surfaces, which is what stops the S3 path and the admin API from disagreeing about who a key belongs to.</p><p>The root user carries a wildcard over buckets as well as a grant on each declared one, so it reaches a bucket created after the registry was built without needing a rebuild to notice.</p>'
    },
    LOOKUP: {
      title: 'Resolve the credential',
      badge: 'storage', badgeText: 'map read',
      body: '<p>A map read, not a scan: lookup cost does not grow with the size of the fleet.</p><p>A key the registry does not hold does <b>not</b> return early. It continues to verification against a fixed dummy secret so the timing of an unknown key matches that of a known one with a wrong secret.</p>'
    },
    VERIFY: {
      title: 'Verify the secret',
      badge: 'filter', badgeText: 'signature',
      body: '<p>For a signed request: reconstruct the canonical request and the string-to-sign, derive the signing key through the HMAC-SHA256 chain <code>secret &rarr; date &rarr; region &rarr; service &rarr; signing key</code>, and compare with <code>crypto/subtle.ConstantTimeCompare</code>.</p><p>The signing key is derived per request rather than cached, so a cache hit cannot make a known key faster than an unknown one. Header auth tolerates &plusmn;15 minutes of clock skew; a presigned URL is checked against its own <code>X-Amz-Expires</code>.</p><p>For a dashboard login: the submitted secret is compared against the stored one, in constant time, with the same dummy-secret equalisation.</p>'
    },
    UNAUTH: {
      title: '401 Unauthorized',
      badge: 'reject', badgeText: 'rejected',
      body: '<p>The caller proved nothing: no signature, an access key the registry does not hold, or a signature that does not verify. All three answer identically, so a refusal does not say whether the key exists.</p><p>Distinct from <code>403</code>, which means the caller proved an identity that holds too little. A client that is authenticated but ungranted gets <code>403</code>, which is the clearer answer for a credential that has been created but not yet granted anything.</p><p>The audit entry for a rejection names no user, because none was proved.</p>'
    },
    WHO: {
      title: 'The authenticated user',
      badge: 'process', badgeText: 'identity',
      body: '<p>From here on the credential is irrelevant: everything is decided by the user it proved and the grants that user holds. Two keypairs belonging to one user reach exactly the same things, which is what makes rotation an overlap rather than a cutover.</p><p>The user id is attached to the request and appears in the audit log, so an action is attributed to the identity that took it rather than to a key several services share.</p>'
    },
    SURFACE: {
      title: 'Which surface',
      badge: 'decision', badgeText: 'dispatch',
      body: '<p>Three surfaces, one grant model. The S3 API and the admin API\'s object routes authorize against bucket grants; the rest of the admin API authorizes against backend and orchestrator grants; the dashboard turns the proved identity into a session.</p><p>The split is in what a route declares it needs, not in how the caller was authenticated. Nothing about the surface changes who the caller is.</p>'
    },
    SESSION: {
      title: 'Session cookie',
      badge: 'success', badgeText: 'dashboard',
      body: '<p>An HMAC-signed cookie carrying the user the credential proved, with a 24-hour TTL. Keys are derived deterministically from <code>ui.session_secret</code>, so sessions survive restarts and instances sharing that value accept each other\'s.</p><p>State-changing dashboard calls also carry a double-submit CSRF token. The <code>Secure</code> flag follows TLS detection or <code>force_secure_cookies</code>, because forcing it unconditionally would make browsers silently drop the cookie on a plain-HTTP hop.</p><p><a href="../../docs/configuration/#ui">Dashboard configuration &rarr;</a></p>'
    },
    BUCKET: {
      title: 'Bucket from the path',
      badge: 'process', badgeText: 'routing',
      body: '<p>The first path segment names the virtual bucket. The admin API\'s object routes take the same namespace, so <code>/admin/api/objects/photos/cat.jpg</code> is authorized against the <code>photos</code> grant exactly as the S3 path would be.</p><p>A prefix naming no single bucket - the empty prefix, or a partial name like <code>pho</code> - cannot be authorized against one grant and is refused. The empty prefix is the whole namespace, and a partial name spans every bucket it prefixes.</p>'
    },
    REACH: {
      title: 'Does the user reach it?',
      badge: 'decision', badgeText: 'grant',
      body: '<p>A named grant on the bucket answers alone. A user holding a wildcard over buckets reaches one it has no named grant on, which is how the root credential reaches a bucket created since the registry was built.</p><p>A named grant is never widened by a wildcard, so a carve-out survives: a user granted <code>read</code> on one bucket and a wildcard elsewhere still only reads that one.</p><p>A user holding no grants at all authenticates and reaches nothing.</p>'
    },
    BITS: {
      title: 'Does the grant carry the bit?',
      badge: 'filter', badgeText: 'permission',
      body: '<p>Six data-plane permissions, as bits 0-5 of a <code>uint64</code>: <code>list-buckets</code>, <code>list</code>, <code>read</code>, <code>write</code>, <code>delete</code>, <code>tags</code>. The operation the request names decides which bits are required, and the check requires <b>all</b> of them.</p><p>A bucket grant recording none carries all six, because rows predate the vocabulary and carried full access before it existed.</p><p>The admin permissions occupy bits 8 and up, a disjoint range, so a data-plane set can never be read as an administrative one by accident.</p>'
    },
    RES: {
      title: 'The resource a route declares',
      badge: 'process', badgeText: 'route table',
      body: '<p>Every control-plane route declares the resource kind and the permission it needs, in the same table the OpenAPI description is generated from. A route added without one authorizes nobody, which is a closed default rather than an open one.</p><p>Three kinds: <code>orchestrator</code> for work belonging to no single provider, <code>backend:name</code> for one provider, and <code>backend:*</code> for all of them.</p><p>A pass that names no backend runs against every one, so it is authorized as <code>backend:*</code>. An operator granted one provider cannot start a conversion that spends egress on the rest of the fleet.</p><p><a href="../../docs/admin-api/#authorization">Admin API authorization &rarr;</a></p>'
    },
    ARES: {
      title: 'Does the user reach that resource?',
      badge: 'decision', badgeText: 'grant',
      body: '<p>A grant on <code>backend:*</code> answers for any named backend; a grant on one named backend does not answer for the wildcard.</p><p>A credential holding only bucket grants is refused on every control-plane route. Bucket grants say nothing about draining a backend, so a data-plane set must not read as one.</p>'
    },
    ABITS: {
      title: 'Does the grant carry the admin bit?',
      badge: 'filter', badgeText: 'permission',
      body: '<p>Ten control-plane permissions: <code>admin-read</code>, <code>admin-logs</code>, <code>admin-maintain</code>, <code>admin-convert</code>, <code>admin-keys</code>, <code>admin-cache</code>, <code>admin-drain</code>, <code>admin-decommission</code>, <code>admin-config</code>, <code>admin-provision</code>.</p><p>They are genuinely separate, not one flag in ten spellings: a credential holding <code>admin-read</code> is refused key rotation, provisioning, log reading and log-level changes alike.</p><p><code>admin-provision</code> is deliberately its own permission rather than part of <code>admin-config</code>, because a grant carrying it can mint a grant carrying anything.</p>'
    },
    FORBID: {
      title: '403 Forbidden',
      badge: 'reject', badgeText: 'rejected',
      body: '<p>The caller proved an identity, and that identity holds too little: no grant on the resource, or a grant that does not carry the permission the operation needs.</p><p>Refused before the operation runs, so a request the store never sees is a request that was refused rather than one that failed.</p><p>Distinct from <code>401</code>, which means nothing was proved at all. Keeping them apart is what lets an operator tell a bad key from an ungranted one.</p>'
    },
    ALLOW: {
      title: 'The request proceeds',
      badge: 'success', badgeText: 'authorized',
      body: '<p>The operation runs, and the audit entry names the user that took it.</p><p>Everything past this point - admission control, routing, replication, the storage layer - treats the request as authorized and never re-derives who the caller is.</p><p><a href="../../docs/authentication/">Authentication reference &rarr;</a></p>'
    }
  };

  var tooltip = document.getElementById('ac-tooltip');
  var mouseX = 0, mouseY = 0;
  var pinned = false, hideTimer = null, hoveringTooltip = false, hoveringNode = false;

  tooltip.addEventListener('mouseenter', function() { hoveringTooltip = true; clearTimeout(hideTimer); });
  tooltip.addEventListener('mouseleave', function() {
    hoveringTooltip = false;
    hideTimer = setTimeout(function() { if (!hoveringNode && !hoveringTooltip) clearInfo(); }, 100);
  });

  document.addEventListener('mousemove', function(e) {
    mouseX = e.clientX; mouseY = e.clientY;
    if (tooltip.style.display === 'block' && !pinned) positionTooltip();
  });
  function positionTooltip() {
    var pad = 12;
    var w = tooltip.offsetWidth, h = tooltip.offsetHeight;
    var vw = window.innerWidth, vh = window.innerHeight;

    var x = mouseX + pad;
    if (x + w > vw - pad) x = mouseX - w - pad;
    x = Math.max(pad, Math.min(x, vw - w - pad));

    // Prefer below the cursor, and flip above only when above genuinely has
    // more room. Clamping afterwards is what keeps a tall panel on screen: an
    // unclamped flip puts its top edge above the viewport, and a panel taller
    // than the viewport pins to the top and scrolls instead.
    var below = vh - mouseY - pad * 2;
    var above = mouseY - pad * 2;
    var y = (h <= below || below >= above) ? mouseY + pad : mouseY - h - pad;
    y = Math.max(pad, Math.min(y, vh - h - pad));

    tooltip.style.left = x + 'px';
    tooltip.style.top = y + 'px';
  }
  function showInfo(id) {
    var info = nodeInfo[id];
    if (!info) { tooltip.style.display = 'none'; pinned = false; return; }
    tooltip.innerHTML = '<h3>' + info.title + '</h3><span class="ac-badge ac-badge-' + info.badge + '">' + info.badgeText + '</span>' + info.body;
    pinned = false;
    tooltip.style.display = 'block'; positionTooltip();
    if (tooltip.querySelector('a')) pinned = true;
  }
  function clearInfo() {
    tooltip.style.display = 'none'; pinned = false;
    var svg = document.querySelector('#ac-diagram svg');
    if (svg) {
      svg.classList.remove('highlighting');
      svg.querySelectorAll('.highlight').forEach(function(el) { el.classList.remove('highlight'); });
    }
  }

  function wireUpInteractivity() {
    var svg = document.querySelector('#ac-diagram svg');
    if (!svg) return;
    var adj = {}, edgeMap = {};
    svg.querySelectorAll('.edgePath').forEach(function(ep, i) {
      var cls = ep.getAttribute('class') || '';
      var m = cls.match(/LS-(\S+)/), m2 = cls.match(/LE-(\S+)/);
      if (!m || !m2) return;
      edgeMap[i] = { from: m[1], to: m2[1], path: ep, label: svg.querySelectorAll('.edgeLabel')[i] };
      (adj[m[1]] = adj[m[1]] || []).push(i);
    });
    function bfs(startId, adjacency, getNext) {
      var visited = new Set([startId]), edges = new Set(), queue = [startId];
      while (queue.length) { var cur = queue.shift(); (adjacency[cur] || []).forEach(function(ei) {
        edges.add(ei); var next = getNext(edgeMap[ei]);
        if (!visited.has(next)) { visited.add(next); queue.push(next); }
      }); } return { nodes: visited, edges: edges };
    }
    var radj = {};
    Object.keys(edgeMap).forEach(function(i) { var e = edgeMap[i]; (radj[e.to] = radj[e.to] || []).push(Number(i)); });
    svg.querySelectorAll('.node').forEach(function(node) {
      var id = node.id.replace(/^flowchart-/, '').replace(/-\d+$/, '');
      node.addEventListener('mouseenter', function() {
        hoveringNode = true; clearTimeout(hideTimer);
        svg.classList.add('highlighting');
        var fwd = bfs(id, adj, function(e) { return e.to; });
        var bwd = bfs(id, radj, function(e) { return e.from; });
        var allNodes = new Set([...fwd.nodes, ...bwd.nodes]);
        var allEdges = new Set([...fwd.edges, ...bwd.edges]);
        svg.querySelectorAll('.node').forEach(function(n) {
          n.classList.toggle('highlight', allNodes.has(n.id.replace(/^flowchart-/, '').replace(/-\d+$/, '')));
        });
        Object.keys(edgeMap).forEach(function(i) {
          var hl = allEdges.has(Number(i));
          edgeMap[i].path.classList.toggle('highlight', hl);
          if (edgeMap[i].label) edgeMap[i].label.classList.toggle('highlight', hl);
        });
        showInfo(id);
      });
      node.addEventListener('mouseleave', function() {
        hoveringNode = false;
        hideTimer = setTimeout(function() { if (!hoveringNode && !hoveringTooltip) clearInfo(); }, 100);
      });
    });
  }
})();
</script>

## How a permission set is stored

A grant records a resource and a permission set. The resource is one of three kinds - `bucket:name`, `backend:name` (or `backend:*`), or `orchestrator` - and the kind decides which vocabulary the set is drawn from: a bucket takes the six data-plane permissions, a backend or the orchestrator takes the ten `admin-*` ones.

The set itself is a single `uint64`. Each permission is one bit, so a grant is one integer column rather than a join table, and authorizing a request is one bitwise `AND` rather than a query.

The two vocabularies occupy **disjoint bits**. The control-plane permissions continue where the data-plane ones stop:

```text
 bit  15 14 13 12 11 10  9  8  7  6    5  4  3  2  1  0
      +-----------------------------+ +-----------------+
      |  ten admin-* permissions    | |  six bucket     |
      |  64 .. 32768                | |  1 .. 32        |
      +-----------------------------+ +-----------------+
```

Which bit a permission sits on carries no meaning beyond being distinct from every other. The stored form is the names - `permissions` is a `TEXT` column holding `list,read`, not an integer - so the bitmask is rebuilt in memory on every registry assembly and a bit could be renumbered without touching a single row.

<style>
  .bittab { width: 100%; border-collapse: collapse; font-size: 0.8rem; margin: 0.75rem 0 1.5rem 0; }
  .bittab th { text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid #30363d; color: #8b949e; font-size: 0.7rem; text-transform: uppercase; letter-spacing: 0.04em; font-weight: 600; }
  .bittab td { padding: 0.35rem 0.6rem; border-bottom: 1px solid #21262d; vertical-align: middle; }
  .bittab tr:last-child td { border-bottom: none; }
  .bittab .c-bit { font-family: ui-monospace, monospace; color: #6e7681; width: 3rem; }
  .bittab .c-mask { font-family: ui-monospace, monospace; color: #8b949e; width: 5.5rem; }
  .bittab .c-dec { font-family: ui-monospace, monospace; color: #a7d5c1; text-align: right; width: 5rem; }
  .bittab .c-name { font-family: ui-monospace, monospace; font-weight: 600; width: 13rem; }
  .bittab .n-data { color: #34b882; }
  .bittab .n-admin { color: #c4a35a; }
  .bittab .c-what { color: #c9d1d9; }
  .bittab tr.total td { border-top: 1px solid #30363d; color: #8b949e; font-style: italic; }
</style>

### The data-plane bits

Carried by a grant on a **bucket**.

<table class="bittab">
  <tr><th>Bit</th><th>Mask</th><th>Value</th><th>Name</th><th>Allows</th></tr>
  <tr><td class="c-bit">0</td><td class="c-mask">0x0001</td><td class="c-dec">1</td><td class="c-name n-data">list-buckets</td><td class="c-what">Knowing the bucket exists - it appears in a bucket listing</td></tr>
  <tr><td class="c-bit">1</td><td class="c-mask">0x0002</td><td class="c-dec">2</td><td class="c-name n-data">list</td><td class="c-what">Enumerating the objects inside it</td></tr>
  <tr><td class="c-bit">2</td><td class="c-mask">0x0004</td><td class="c-dec">4</td><td class="c-name n-data">read</td><td class="c-what">Fetching an object body, and presigning one</td></tr>
  <tr><td class="c-bit">3</td><td class="c-mask">0x0008</td><td class="c-dec">8</td><td class="c-name n-data">write</td><td class="c-what">Storing an object, copying one in, running a multipart upload</td></tr>
  <tr><td class="c-bit">4</td><td class="c-mask">0x0010</td><td class="c-dec">16</td><td class="c-name n-data">delete</td><td class="c-what">Removing a key, a batch of them, or a whole prefix</td></tr>
  <tr><td class="c-bit">5</td><td class="c-mask">0x0020</td><td class="c-dec">32</td><td class="c-name n-data">tags</td><td class="c-what">Reading and writing an object's tag set</td></tr>
  <tr class="total"><td class="c-bit"></td><td class="c-mask">0x003F</td><td class="c-dec">63</td><td class="c-name">all</td><td class="c-what">Every data-plane permission, and what an empty bucket grant carries</td></tr>
</table>

`list-buckets` and `list` are separate because they answer different questions: a client may be entitled to know a bucket exists without being entitled to enumerate what is in it. It is the same split AWS draws between `s3:ListAllMyBuckets` and `s3:ListBucket`.

### The control-plane bits

Carried by a grant on a **backend** or on the **orchestrator**.

<table class="bittab">
  <tr><th>Bit</th><th>Mask</th><th>Value</th><th>Name</th><th>Allows</th></tr>
  <tr><td class="c-bit">6</td><td class="c-mask">0x0040</td><td class="c-dec">64</td><td class="c-name n-admin">admin-read</td><td class="c-what">Status, worker health, reload status, object locations, drain status</td></tr>
  <tr><td class="c-bit">7</td><td class="c-mask">0x0080</td><td class="c-dec">128</td><td class="c-name n-admin">admin-logs</td><td class="c-what">Reading buffered log entries</td></tr>
  <tr><td class="c-bit">8</td><td class="c-mask">0x0100</td><td class="c-dec">256</td><td class="c-name n-admin">admin-maintain</td><td class="c-what">Repair passes: scrub, checksum backfill, reconcile, replicate, rebalance</td></tr>
  <tr><td class="c-bit">9</td><td class="c-mask">0x0200</td><td class="c-dec">512</td><td class="c-name n-admin">admin-convert</td><td class="c-what">Rewrite passes: encrypt, decrypt, compress and decompress existing objects</td></tr>
  <tr><td class="c-bit">10</td><td class="c-mask">0x0400</td><td class="c-dec">1024</td><td class="c-name n-admin">admin-keys</td><td class="c-what">Rotating the encryption master key</td></tr>
  <tr><td class="c-bit">11</td><td class="c-mask">0x0800</td><td class="c-dec">2048</td><td class="c-name n-admin">admin-cache</td><td class="c-what">Flushing and invalidating the object data cache</td></tr>
  <tr><td class="c-bit">12</td><td class="c-mask">0x1000</td><td class="c-dec">4096</td><td class="c-name n-admin">admin-drain</td><td class="c-what">Starting and cancelling a backend drain</td></tr>
  <tr><td class="c-bit">13</td><td class="c-mask">0x2000</td><td class="c-dec">8192</td><td class="c-name n-admin">admin-decommission</td><td class="c-what">Removing a backend, including purging its objects</td></tr>
  <tr><td class="c-bit">14</td><td class="c-mask">0x4000</td><td class="c-dec">16384</td><td class="c-name n-admin">admin-config</td><td class="c-what">Changing runtime configuration, such as the log level</td></tr>
  <tr><td class="c-bit">15</td><td class="c-mask">0x8000</td><td class="c-dec">32768</td><td class="c-name n-admin">admin-provision</td><td class="c-what">Creating and removing buckets, users, credentials and grants</td></tr>
  <tr class="total"><td class="c-bit"></td><td class="c-mask">0xFFC0</td><td class="c-dec">65472</td><td class="c-name">admin-all</td><td class="c-what">Every control-plane permission</td></tr>
</table>

Each is split from its neighbour where a real principal wants one and not the other: a monitoring credential reads status but not logs, an on-call engineer runs the repair passes but does not rewrite every object. `admin-provision` is alone because a grant carrying it can mint any other permission.

### Setting bits

A grant is built by OR-ing together the bits it was written with. `grant add -permissions list,read` combines two masks into the one integer the `permissions` column holds:

```text
     list        000010       2
  OR read        000100       4
     ------------------------------
   = stored      000110       6
```

Rendering it back is the reverse walk over the same table, which is why a set always prints in one fixed order and two equal grants read identically.

### Checking bits

A route declares what it needs. Authorizing is one `AND`, and the result has to equal what was wanted rather than merely be non-zero:

```go
func (p PermissionSet) Has(want PermissionSet) bool {
	return p&want == want
}
```

A `GET` on an object needs `read`, and the grant above carries it:

```text
     held        000110       6     list,read
 AND want        000100       4     read
     ------------------------------
   = result      000100       4     == want   -> allowed
```

A `DELETE` on the same bucket needs `delete`, which that grant does not carry:

```text
     held        000110       6     list,read
 AND want        010000      16     delete
     ------------------------------
   = result      000000       0     != want   -> 403
```

The `== want` rather than `!= 0` matters whenever a route declares more than one bit: a grant holding one of two required permissions is refused, which is the difference between "may do any of these" and "may do this operation".

### Why the two vocabularies cannot be confused

Take the most permissive bucket grant there is - `all`, every data-plane bit lit - and check it against a control-plane permission:

```text
                  admin        bucket
     held       0000000000     111111        63    bucket grant: all
 AND want       0000010000     000000      4096    admin-drain
     ----------------------------------------
   = result     0000000000     000000         0    != want  -> 403
```

The two operands share no bit, so the result is zero no matter how much access the bucket grant carries. There is no combination of data-plane permissions that adds up to an administrative one, and none that can be reached by arithmetic on a stored value.

In practice that check never even runs. `ValidatePermissions` refuses a set carrying permissions the resource has no meaning for when the grant is **written** rather than when it is checked, so `grant add -name photos -permissions admin-drain` is rejected outright rather than stored as a grant that would authorize nothing. The disjoint bits are the second line of defence behind that, and the reason a hand-edited row still cannot cross the line.

### What an empty set means

An empty set is read differently on each side, and deliberately so.

An empty **bucket** grant carries all six data-plane bits (`63`). Rows predate the permission vocabulary and authorized everything within the bucket they named, so reading them as nothing on upgrade would have refused clients that were working the day before.

An empty **backend or orchestrator** grant carries nothing (`0`). No control-plane row predates this vocabulary, so there is no prior meaning to preserve, and defaulting one to everything is the wrong direction to be generous in.

### Combining grants

Merging two sets is an `OR`, which is how a user's grants from config and from the store come together. A wildcard grant is the exception: a **named** grant replaces the wildcard for the bucket it names rather than being OR-ed into it.

That has to be a replacement, because OR-ing would make a carve-out impossible to express. A user granted broad `read` across every bucket could never be narrowed to `list-buckets` on the one holding secrets - the broader bit would always survive the union.

## Legend

| Color | Meaning |
|-------|---------|
| <span style="color:#1a7a5a">**Forest green**</span> | Entry point / source |
| <span style="color:#c4a35a">**Amber**</span> | Verification / permission check |
| <span style="color:#2a9d73">**Green border**</span> | Decision / branch |
| <span style="color:#5ec9a0">**Teal**</span> | Processing step |
| <span style="color:#4aaa8a">**Teal**</span> | Registry / metadata store |
| <span style="color:#34b882">**Green**</span> | Authorized |
| <span style="color:#d4a0a0">**Red**</span> | Rejected |

## See also

- [Setting up access control](../../guides/access-control/) - the walkthrough
- [Authentication reference](../../docs/authentication/) - the credential-to-user-to-grant chain in prose
- [Admin API authorization](../../docs/admin-api/#authorization) - the permission each endpoint declares
- [Admission control](../admission-control/) - where authentication sits in the request lifecycle
- [Database schema](../database-schema/) - the `users`, `credentials` and `grants` tables
