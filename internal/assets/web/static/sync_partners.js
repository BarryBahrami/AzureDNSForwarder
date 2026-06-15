(function () {
  const $ = (s) => document.querySelector(s);
  const tbody = $('#peers tbody');

  function fmtTime(t) {
    if (!t) return '—';
    const d = new Date(t);
    if (isNaN(d.getTime())) return '—';
    const delta = Math.floor((Date.now() - d.getTime()) / 1000);
    if (delta < 60) return delta + 's ago';
    if (delta < 3600) return Math.floor(delta / 60) + 'm ago';
    if (delta < 86400) return Math.floor(delta / 3600) + 'h ago';
    return d.toLocaleString();
  }

  function statusBadge(peer) {
    if (!peer.enabled) return '<span class="badge disabled">disabled</span>';
    if (peer.status && peer.status.last_error) return '<span class="badge err">error</span>';
    if (peer.status && peer.status.last_contact) return '<span class="badge ok">healthy</span>';
    return '<span class="badge warn">pending</span>';
  }

  function row(p) {
    const tr = document.createElement('tr');
    const action = (p.status && p.status.last_action) ? p.status.last_action : '—';
    const err = (p.status && p.status.last_error) ? p.status.last_error : '';
    const seq = (p.status && p.status.last_seq) ? p.status.last_seq : '—';
    tr.innerHTML = `
      <td><code>${escapeHTML(p.name)}</code></td>
      <td><code>${escapeHTML(p.url)}</code></td>
      <td>${statusBadge(p)}</td>
      <td>${fmtTime(p.status && p.status.last_contact)}</td>
      <td>${seq}</td>
      <td>${escapeHTML(action)}</td>
      <td class="err">${escapeHTML(err)}</td>
      <td>
        <button data-act="toggle" data-name="${escapeHTML(p.name)}" data-enabled="${p.enabled ? '1' : '0'}">${p.enabled ? 'disable' : 'enable'}</button>
        <button data-act="sync" data-name="${escapeHTML(p.name)}">Sync now</button>
        <button data-act="del" data-name="${escapeHTML(p.name)}">delete</button>
      </td>
    `;
    return tr;
  }

  function escapeHTML(s) {
    return String(s || '').replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    })[c]);
  }

  async function j(method, url, body) {
    const opts = { method, headers: {} };
    if (body) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const r = await fetch(url, opts);
    if (!r.ok) {
      const t = await r.text();
      throw new Error(t || (r.status + ' ' + r.statusText));
    }
    return r.json();
  }

  async function refresh() {
    try {
      const data = await j('GET', '/api/peers/status');
      tbody.innerHTML = '';
      for (const p of data.peers || []) {
        tbody.appendChild(row(p));
      }
    } catch (err) {
      console.error('refresh peers', err);
    }
  }

  $('#add-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const body = {
      name: (fd.get('name') || '').toString().trim(),
      url: (fd.get('url') || '').toString().trim(),
      enabled: !!fd.get('enabled'),
    };
    try {
      await j('POST', '/api/peers', body);
      e.target.reset();
      e.target.querySelector('input[name=enabled]').checked = true;
      refresh();
    } catch (err) {
      alert(err.message);
    }
  });

  tbody.addEventListener('click', async (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    const name = btn.dataset.name;
    const act = btn.dataset.act;
    if (!name || !act) return;
    try {
      if (act === 'toggle') {
        const enabled = btn.dataset.enabled !== '1';
        await j('PUT', '/api/peers/' + encodeURIComponent(name), { enabled });
      } else if (act === 'del') {
        if (!confirm('Remove peer ' + name + '?')) return;
        await j('DELETE', '/api/peers/' + encodeURIComponent(name));
      } else if (act === 'sync') {
        const r = await j('POST', '/api/peers/' + encodeURIComponent(name) + '/sync');
        if (r.message) alert(r.message);
      }
      refresh();
    } catch (err) {
      alert(err.message);
    }
  });

  refresh();
  setInterval(refresh, 5000);
})();
