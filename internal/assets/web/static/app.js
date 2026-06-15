// Tiny framework-free helpers: fetch, render rows, manage forms.
const $ = (s, r=document) => r.querySelector(s);
const $$ = (s, r=document) => Array.from(r.querySelectorAll(s));
const j = (method, url, body) => fetch(url, {method, headers: {'Content-Type':'application/json'}, body: body?JSON.stringify(body):undefined}).then(r => { if(!r.ok) return r.text().then(t => Promise.reject(new Error(`${r.status}: ${t}`))); return r.json(); });

function escapeHTML(s) {
  return String(s || '').replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[c]);
}

async function loadStatus() {
  try {
    const s = await j('GET', '/api/status');
    const line = $('#status-line');
    if (line) {
      line.innerHTML = `v${s.version} · ${s.last_error ? `<span class="err">⚠ ${s.last_error}</span>` : '<span class="ok">● healthy</span>'} · ${s.hash ? s.hash.slice(0,8) : '—'}`;
    }
  } catch (e) { /* ignore */ }
}

document.addEventListener('DOMContentLoaded', () => {
  loadStatus();
  setInterval(loadStatus, 5000);
  hookDashboard();
  hookForwarders();
  hookRecords();
  hookSettings();
  hookImport();
  hookAudit();
});

function hookDashboard() {
  const form = $('#test-form');
  if (!form) return;
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(form);
    const body = Object.fromEntries(fd.entries());
    body.qtype = body.qtype || 'A';
    const out = $('#test-result');
    out.textContent = 'resolving…';
    try {
      const r = await j('POST', '/api/test', body);
      out.textContent = JSON.stringify(r, null, 2);
    } catch (err) { out.textContent = 'error: ' + err.message; }
  });
}

function hookForwarders() {
  const tbody = $('#zones tbody');
  const defTbody = $('#defaults tbody');
  if (!tbody) return;

  function fmtSync(doNotSync, peers) {
    if (doNotSync) return '<span class="badge disabled">local only</span>';
    if (!peers || peers.length === 0) return '<span class="badge ok">all peers</span>';
    return '<span class="badge warn">' + escapeHTML(peers.join(', ')) + '</span>';
  }

  async function refresh() {
    const cfg = await j('GET', '/api/config');
    const peers = (cfg.peers && cfg.peers.list) || [];
    const peerNames = peers.map(p => p.name).filter(Boolean);
    tbody.innerHTML = '';
    for (const z of cfg.forward_zones) {
      if (z.deleted) continue;
      const latencyBadge = z.least_latency
        ? `<span class="badge ok">ll ${z.latency_test_frequency || 5}m</span>`
        : '';
      const tr = document.createElement('tr');
      tr.innerHTML = `<td>${escapeHTML(z.name)}</td><td>${z.wildcard ? '✓' : ''}</td><td>${escapeHTML((z.upstreams||[]).join(', '))}</td><td>${latencyBadge}</td><td>${fmtSync(z.do_not_sync, z.sync_peers)}</td><td><button data-id="${escapeHTML(z.id)}">delete</button></td>`;
      tbody.appendChild(tr);
    }
    defTbody.innerHTML = '';
    for (const u of cfg.upstream_defaults) {
      if (u.deleted) continue;
      const tr = document.createElement('tr');
      tr.innerHTML = `
        <td><input type="checkbox" ${u.enabled ? 'checked' : ''} data-addr="${escapeHTML(u.address)}" data-port="${u.port}" data-act="toggle"></td>
        <td>${escapeHTML(u.address)}</td>
        <td>${u.port}</td>
        <td>${escapeHTML(u.note || '')}</td>
        <td class="sync-cell">${fmtSync(u.do_not_sync, u.sync_peers)}</td>
        <td>
          <span class="sync-edit" style="display:none">
            <input type="text" value="${escapeHTML((u.sync_peers || []).join(', '))}" data-addr="${escapeHTML(u.address)}" data-port="${u.port}">
            <button data-act="edit-peers" data-addr="${escapeHTML(u.address)}" data-port="${u.port}">save</button>
          </span>
          <button data-act="delete" data-addr="${escapeHTML(u.address)}" data-port="${u.port}">delete</button>
        </td>
      `;
      const editBtn = document.createElement('button');
      editBtn.textContent = 'sync';
      editBtn.dataset.act = 'toggle-edit';
      editBtn.dataset.addr = u.address;
      editBtn.dataset.port = u.port;
      tr.querySelector('td:last-child').prepend(editBtn);
      defTbody.appendChild(tr);
    }
  }

  tbody.addEventListener('click', async (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    const id = btn.dataset.id;
    const act = btn.dataset.act;
    // Forwarder delete buttons do not have data-act; they only have data-id.
    if (!id || act) return;
    try {
      await j('DELETE', '/api/forwarders/' + encodeURIComponent(id));
      refresh();
    } catch (err) { alert(err.message); }
  });
  defTbody.addEventListener('click', async (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    const addr = btn.dataset.addr;
    const port = btn.dataset.port;
    const act = btn.dataset.act;
    if (!addr || !port) return;
    try {
      if (act === 'delete') {
        await j('DELETE', `/api/defaults/${encodeURIComponent(addr)}/${port}`);
      } else if (act === 'edit-peers') {
        const row = btn.closest('tr');
        const input = row.querySelector('.sync-edit input');
        const peers = input.value.split(',').map(s => s.trim()).filter(Boolean);
        await j('PATCH', `/api/defaults/${encodeURIComponent(addr)}/${port}`, { sync_peers: peers });
        const edit = row.querySelector('.sync-edit');
        const cell = row.querySelector('.sync-cell');
        const toggleBtn = row.querySelector('button[data-act="toggle-edit"]');
        edit.style.display = 'none';
        cell.style.display = '';
        if (toggleBtn) toggleBtn.textContent = 'sync';
      } else if (act === 'toggle-edit') {
        const row = btn.closest('tr');
        const edit = row.querySelector('.sync-edit');
        const cell = row.querySelector('.sync-cell');
        const wasHidden = edit.style.display === 'none';
        edit.style.display = wasHidden ? 'inline-flex' : 'none';
        cell.style.display = wasHidden ? 'none' : '';
        btn.textContent = wasHidden ? 'cancel' : 'sync';
      }
      refresh();
    } catch (err) { alert(err.message); }
  });
  defTbody.addEventListener('change', async (e) => {
    if (e.target.matches('input[type=checkbox][data-act=toggle]')) {
      const enabled = e.target.checked;
      try {
        await j('PATCH', `/api/defaults/${encodeURIComponent(e.target.dataset.addr)}/${e.target.dataset.port}`, {enabled});
        refresh();
      } catch (err) {
        alert(err.message);
        e.target.checked = !enabled;
      }
    }
  });

  // forwarder form with cloud DNS toggles
  const fwdForm = $('#add-form');
  const fwdName = fwdForm.querySelector('input[name=name]');
  const azureFwd = fwdForm.querySelector('input[name=send_to_azure]');
  const awsFwd = fwdForm.querySelector('input[name=send_to_aws]');
  const gcpFwd = fwdForm.querySelector('input[name=send_to_gcp]');
  const upInput = fwdForm.querySelector('input[name=upstreams]');
  const llCheck = fwdForm.querySelector('#ll-check');
  const llLabel = fwdForm.querySelector('#ll-label');
  const llFreq = fwdForm.querySelector('#ll-freq');
  const llFreqLabel = fwdForm.querySelector('#ll-freq-label');
  const fwdPeersInput = fwdForm.querySelector('input[name=sync_peers]');
  if (fwdPeersInput) fwdPeersInput.placeholder = 'peer names (default: all)';

  function updateLLAvailability() {
    const name = (fwdName.value || '').trim();
    const exact = name && !name.startsWith('*.');
    llCheck.disabled = !exact;
    llFreq.disabled = !exact;
    llLabel.style.opacity = exact ? '' : '0.4';
    llFreqLabel.style.opacity = exact ? '' : '0.4';
    if (!exact) {
      llCheck.checked = false;
    }
  }
  fwdName.addEventListener('input', updateLLAvailability);
  updateLLAvailability();

  llCheck.addEventListener('change', () => {
    if (!llCheck.checked) {
      llFreq.value = '5';
    }
  });
  const cloudDnsFwd = { azure: '168.63.129.16', aws: '169.254.169.253', gcp: '169.254.169.254' };
  function syncFwdCloud() {
    const provider = azureFwd.checked ? 'azure' : awsFwd.checked ? 'aws' : gcpFwd.checked ? 'gcp' : null;
    upInput.disabled = !!provider;
    if (provider) {
      upInput.placeholder = `(forwarding to ${provider === 'azure' ? 'Azure' : provider === 'aws' ? 'AWS' : 'GCP'} DNS)`;
    } else {
      upInput.placeholder = '10.0.0.4, 10.0.0.5';
    }
    for (const [key, box] of Object.entries({ azure: azureFwd, aws: awsFwd, gcp: gcpFwd })) {
      if (key !== provider) box.checked = false;
    }
  }
  for (const box of [azureFwd, awsFwd, gcpFwd]) {
    box.addEventListener('change', syncFwdCloud);
  }
  syncFwdCloud();

  fwdForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(fwdForm);
    const name = (fd.get('name') || '').trim();
    if (!name) { alert('name is required'); return; }
    // Wildcard is auto-detected from a leading "*." prefix.
    const wildcard = name.startsWith('*.');
    const provider = !!fd.get('send_to_azure') ? 'azure' : !!fd.get('send_to_aws') ? 'aws' : !!fd.get('send_to_gcp') ? 'gcp' : null;
    let upstreams;
    if (provider) {
      upstreams = [cloudDnsFwd[provider]];
    } else {
      const upstreamStr = (fd.get('upstreams') || '').trim();
      upstreams = upstreamStr ? upstreamStr.split(',').map(s=>s.trim()).filter(Boolean) : [];
    }
    if (upstreams.length === 0) {
      alert('enter at least one upstream, or check a cloud DNS option');
      return;
    }
    const exact = !wildcard;
    if (!exact && !!fd.get('least_latency')) {
      alert('least latency is only available for exact (non-wildcard) names');
      return;
    }
    try {
      const doNotSync = !!fd.get('do_not_sync');
      const syncPeers = doNotSync ? [] : (fwdPeersInput ? (fwdPeersInput.value || '').split(',').map(s=>s.trim()).filter(Boolean) : []);
      const leastLatency = exact && !!fd.get('least_latency');
      const latencyFreq = parseInt(fd.get('latency_test_frequency') || '5', 10);
      await j('POST', '/api/forwarders', {
        name, wildcard, upstreams,
        least_latency: leastLatency,
        latency_test_frequency: latencyFreq,
        do_not_sync: doNotSync, sync_peers: syncPeers
      });
      fwdForm.reset();
      syncFwdCloud();
      updateLLAvailability();
      refresh();
    } catch (err) { alert(err.message); }
  });

  // defaults form with cloud DNS toggles
  const defForm = $('#defaults-form');
  const azureBox = defForm.querySelector('input[name=use_azure]');
  const awsBox = defForm.querySelector('input[name=use_aws]');
  const gcpBox = defForm.querySelector('input[name=use_gcp]');
  const addrInput = defForm.querySelector('input[name=address]');
  const portInput = defForm.querySelector('input[name=port]');
  const defPeersInput = defForm.querySelector('input[name=sync_peers]');
  const defNoteInput = defForm.querySelector('input[name=note]');
  if (defPeersInput) defPeersInput.placeholder = 'peer names (default: all)';
  const cloudDnsDef = { azure: { address: '168.63.129.16', note: 'Azure DNS' }, aws: { address: '169.254.169.253', note: 'AWS DNS' }, gcp: { address: '169.254.169.254', note: 'GCP DNS' } };
  function syncCloud() {
    const provider = azureBox.checked ? 'azure' : awsBox.checked ? 'aws' : gcpBox.checked ? 'gcp' : null;
    addrInput.disabled = !!provider;
    portInput.disabled = !!provider;
    addrInput.placeholder = provider ? `(using ${cloudDnsDef[provider].note})` : 'e.g. 8.8.8.8';
    for (const [key, box] of Object.entries({ azure: azureBox, aws: awsBox, gcp: gcpBox })) {
      if (key !== provider) box.checked = false;
    }
  }
  for (const box of [azureBox, awsBox, gcpBox]) {
    box.addEventListener('change', syncCloud);
  }
  syncCloud();

  defForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(defForm);
    const provider = !!fd.get('use_azure') ? 'azure' : !!fd.get('use_aws') ? 'aws' : !!fd.get('use_gcp') ? 'gcp' : null;
    const preset = provider ? cloudDnsDef[provider] : null;
    const doNotSync = !!fd.get('do_not_sync');
    const syncPeers = doNotSync ? [] : (defPeersInput ? (defPeersInput.value || '').split(',').map(s=>s.trim()).filter(Boolean) : []);
    const body = {
      address: preset ? preset.address : (fd.get('address') || '').trim(),
      port: parseInt(fd.get('port') || '53', 10),
      note: (fd.get('note') || '').trim() || (preset ? preset.note : ''),
      enabled: true,
      do_not_sync: doNotSync,
      sync_peers: syncPeers,
    };
    if (!body.address) { alert('enter a nameserver or check a cloud DNS option'); return; }
    try {
      await j('POST', '/api/defaults', body);
      defForm.reset();
      syncCloud();
      refresh();
    } catch (err) { alert(err.message); }
  });

  refresh();
}

function hookRecords() {
  const tbody = $('#recs tbody');
  if (!tbody) return;
  async function refresh() {
    const recs = await j('GET', '/api/records');
    tbody.innerHTML = '';
    for (const r of recs) {
      if (r.deleted) continue;
      const tr = document.createElement('tr');
      const syncBadge = r.do_not_sync
        ? '<span class="badge disabled">private</span>'
        : (!r.sync_peers || r.sync_peers.length === 0)
        ? '<span class="badge ok">all peers</span>'
        : '<span class="badge warn">' + escapeHTML(r.sync_peers.join(', ')) + '</span>';
      tr.innerHTML = `<td>${escapeHTML(r.name)}</td><td>${escapeHTML(r.type)}</td><td>${escapeHTML(r.value)}</td><td>${r.ttl||''}</td><td>${syncBadge}</td><td><button data-id="${r.id}">delete</button></td>`;
      tbody.appendChild(tr);
    }
  }
  tbody.addEventListener('click', async (e) => {
    if (e.target.tagName === 'BUTTON') {
      try {
        await j('DELETE', '/api/records/' + e.target.dataset.id);
        refresh();
      } catch (err) { alert(err.message); }
    }
  });
  const form = $('#add-form');
  const recPeersInput = form.querySelector('input[name=sync_peers]');
  if (recPeersInput) recPeersInput.placeholder = 'peer names (default: all)';
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(form);
    try {
      await j('POST', '/api/records', {
        name: (fd.get('name') || '').trim(),
        type: (fd.get('type') || 'A').toUpperCase(),
        value: (fd.get('value') || '').trim(),
        ttl: parseInt(fd.get('ttl') || '300', 10),
        do_not_sync: !!fd.get('do_not_sync'),
        sync_peers: recPeersInput ? (recPeersInput.value || '').split(',').map(s=>s.trim()).filter(Boolean) : [],
      });
      form.reset();
      refresh();
    } catch (err) { alert(err.message); }
  });
  refresh();
}

function hookSettings() {
  const form = $('#settings-form');
  if (!form) return;
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(form);
    const body = {
      cache_size: parseInt(fd.get('cache_size') || '1000', 10),
      dnssec: !!fd.get('dnssec'),
      log_queries: !!fd.get('log_queries'),
      dns_listen: fd.get('dns_listen') || '0.0.0.0:53',
      http_listen: fd.get('http_listen') || '0.0.0.0:80',
      poll_seconds: parseInt(fd.get('poll_seconds') || '10', 10),
    };
    try { await j('PUT', '/api/settings', body); alert('saved'); }
    catch (err) { alert(err.message); }
  });
}

function hookImport() {
  const form = $('#import-form');
  if (!form) return;
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const data = new FormData(form);
    const r = await fetch('/api/import', {method:'POST', body: data});
    if (r.ok) { alert('imported'); location.reload(); }
    else { alert(await r.text()); }
  });
}

function hookAudit() {
  const tbody = $('#audit tbody');
  if (!tbody) return;
  j('GET','/api/audit').then(entries => {
    tbody.innerHTML = '';
    for (const e of entries.reverse()) {
      const tr = document.createElement('tr');
      tr.innerHTML = `<td>${e.time}</td><td>${e.actor||''}</td><td>${e.action}</td><td>${e.details||''}</td>`;
      tbody.appendChild(tr);
    }
  });
}
