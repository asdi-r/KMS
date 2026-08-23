// Shared helpers for NETRA KMS portals. API is same-origin (via Kong).
const KMS = (() => {
  // JWT session (sessionStorage: cleared when the tab closes)
  const session = {
    get: () => { try { return JSON.parse(sessionStorage.getItem('kms_session')) || null; } catch { return null; } },
    set: v => sessionStorage.setItem('kms_session', JSON.stringify(v)),
    clear: () => sessionStorage.removeItem('kms_session'),
    token: () => session.get()?.token || '',
    user: () => session.get()?.user || null,
  };

  async function api(method, path, body, { auth = true } = {}) {
    const headers = { 'Content-Type': 'application/json' };
    if (auth && session.token()) headers['Authorization'] = 'Bearer ' + session.token();
    const res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
    let data = null;
    try { data = await res.json(); } catch { /* empty body */ }
    if (!res.ok) {
      const err = new Error((data && (data.error || data.reason)) || `HTTP ${res.status}`);
      err.status = res.status; err.data = data;
      throw err;
    }
    return data;
  }

  const fmtDate = iso => iso ? new Date(iso).toLocaleDateString('id-ID', { year: 'numeric', month: 'short', day: '2-digit' }) : '—';
  const fmtDT = iso => iso ? new Date(iso).toLocaleString('id-ID', { dateStyle: 'medium', timeStyle: 'short' }) : '—';
  const daysLeft = iso => Math.ceil((new Date(iso) - Date.now()) / 864e5);
  const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  function statusBadge(k) {
    if (k.status !== 'active') return `<span class="badge bad">${esc(k.status)}</span>`;
    const d = daysLeft(k.expires_at);
    if (d < 0) return `<span class="badge bad">expired</span>`;
    if (d <= 60) return `<span class="badge warn">expiring ${d}d</span>`;
    return `<span class="badge active">active</span>`;
  }

  function meter(used, seats) {
    const pct = seats ? Math.min(100, Math.round(used / seats * 100)) : 0;
    return `<div class="meter ${used >= seats ? 'full' : ''}"><i style="width:${pct}%"></i></div>`;
  }

  // Decision ribbon messaging
  let ribbonTimer;
  function ribbon(msg, kind = '') {
    const el = document.getElementById('ribbon-msg');
    if (!el) return;
    el.textContent = msg; el.className = 'msg ' + kind;
    clearTimeout(ribbonTimer);
    if (kind === 'ok' || kind === 'err') ribbonTimer = setTimeout(() => { el.textContent = 'DECISION RIBBON — idle.'; el.className = 'msg'; }, 6000);
  }

  function copy(text) {
    navigator.clipboard?.writeText(text).then(() => ribbon('Copied to clipboard: ' + text, 'ok'));
  }

  // Autonomy mode switcher (purely visual per design language)
  function initModes() {
    const saved = localStorage.getItem('kms_mode') || 'guided';
    document.body.setAttribute('data-autonomy-mode', saved);
    document.querySelectorAll('.modes button').forEach(b => {
      b.classList.toggle('active', b.dataset.mode === saved);
      b.addEventListener('click', () => {
        document.body.setAttribute('data-autonomy-mode', b.dataset.mode);
        localStorage.setItem('kms_mode', b.dataset.mode);
        document.querySelectorAll('.modes button').forEach(x => x.classList.toggle('active', x === b));
      });
    });
  }

  return { api, session, fmtDate, fmtDT, daysLeft, esc, statusBadge, meter, ribbon, copy, initModes };
})();

// ---- CSV export (client-side) ----
KMS.csv = {
  build(rows, columns) {
    const q = v => { const s = v == null ? '' : String(v); return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s; };
    return [columns.map(c => q(c.label)).join(','), ...rows.map(r => columns.map(c => q(typeof c.value === 'function' ? c.value(r) : r[c.value])).join(','))].join('\r\n');
  },
  download(filename, rows, columns) {
    const blob = new Blob(['﻿' + KMS.csv.build(rows, columns)], { type: 'text/csv;charset=utf-8' });
    const a = document.createElement('a'); a.href = URL.createObjectURL(blob); a.download = filename; a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    KMS.ribbon?.('Exported ' + filename + ' (' + rows.length + ' rows)', 'ok');
  },
};

// ---- autocomplete: attaches a <datalist> to an input; fetches after minChars ----
KMS.autocomplete = function (input, fetchOptions, { minChars = 3, delay = 250 } = {}) {
  const id = 'dl-' + Math.random().toString(36).slice(2);
  const dl = document.createElement('datalist'); dl.id = id; document.body.appendChild(dl);
  input.setAttribute('list', id); input.setAttribute('autocomplete', 'off');
  let timer, last = '';
  input.addEventListener('input', () => {
    const q = input.value.trim();
    clearTimeout(timer);
    if (q.length < minChars) { dl.innerHTML = ''; last = ''; return; }
    if (q === last) return;
    timer = setTimeout(async () => {
      try {
        const opts = await fetchOptions(q); last = q;
        dl.innerHTML = opts.map(o => typeof o === 'string' ? `<option value="${KMS.esc(o)}"></option>` : `<option value="${KMS.esc(o.value)}">${KMS.esc(o.label || '')}</option>`).join('');
      } catch { /* ignore */ }
    }, delay);
  });
};
