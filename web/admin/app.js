// Gateway admin single-page app. Vanilla ES module: no framework, no build
// step, no third-party origin. Every read and write goes through /admin/api/*.

const view = document.getElementById('view');
const titleEl = document.getElementById('title');
const subEl = document.getElementById('sub');
const toastEl = document.getElementById('toast');

const PAGES = {
  dashboard: ['Stats', 'Usage, latency and request history for every upstream API.'],
  models: ['Models', 'Upstream providers and the model catalog.'],
  config: ['Config', 'Auto model, model cycle, web search.'],
  chromes: ['Captcha Chrome', 'Captcha Chrome processes — start on demand, kill to free RAM.'],
};

let renderGen = 0;
let toastTimer;

const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
}[c]));

function toast(msg) {
  toastEl.textContent = msg;
  toastEl.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toastEl.classList.remove('show'), 1600);
}

async function api(path, opts = {}) {
  const init = { credentials: 'same-origin', ...opts };
  if (init.body != null && typeof init.body !== 'string') {
    init.body = JSON.stringify(init.body);
    init.headers = { 'content-type': 'application/json', ...(init.headers || {}) };
  }
  const res = await fetch(path, init);
  if (res.status === 401) {
    location.href = '/admin/login';
    throw new Error('Sign in required');
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = text; }
  }
  if (!res.ok) {
    const msg = (data && data.error) || (typeof data === 'string' && data) || ('HTTP ' + res.status);
    throw new Error(msg);
  }
  return data;
}

const emptyRow = msg => `<div class="empty">${esc(msg)}</div>`;
const badge = (cls, text) => `<span class="badge ${cls}">${esc(text)}</span>`;

// --- routing -------------------------------------------------------------

function pageFromURL() {
  const p = new URLSearchParams(location.search).get('page');
  if (p === 'automode') return 'config';
  return PAGES[p] ? p : 'dashboard';
}

async function route() {
  const page = pageFromURL();
  const gen = ++renderGen;
  document.querySelectorAll('.rail-links a').forEach(a => {
    const on = a.dataset.page === page;
    a.classList.toggle('on', on);
    if (on) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
  titleEl.textContent = PAGES[page][0];
  subEl.textContent = PAGES[page][1];
  view.innerHTML = '<div class="view-msg">Loading…</div>';
  try {
    if (page === 'dashboard') await renderDashboard(gen);
    else if (page === 'models') await renderModels(gen);
    else if (page === 'config') await renderConfig(gen);
    else await renderChromes(gen);
  } catch (err) {
    if (gen === renderGen) {
      view.innerHTML = `<section class="sheet"><div class="sheet-body view-msg">${esc(err.message)}</div></section>`;
      toast(err.message);
    }
  }
}

document.querySelectorAll('.rail-links a').forEach(a => a.addEventListener('click', e => {
  if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
  e.preventDefault();
  if (a.dataset.page === pageFromURL()) return;
  history.pushState({ page: a.dataset.page }, '', '?page=' + a.dataset.page);
  route();
}));
window.addEventListener('popstate', route);

// --- dashboard -----------------------------------------------------------

async function renderDashboard(gen) {
  view.innerHTML = `
    <section class="sec"><h2>Traffic</h2><div id="charts"></div></section>
    <section class="sheet"><div class="sheet-head"><h2>Request log</h2><div id="frames"></div></div><div id="framebox"></div></section>`;
  const charts = document.getElementById('charts');
  const framesBox = document.getElementById('frames');
  const frameBox = document.getElementById('framebox');
  const INTERVAL = 1500;

  const fmtTime = t => { const d = new Date(t); return d.getFullYear() > 1 ? d.toLocaleTimeString() : '—'; };
  const fmtMS = ms => ms >= 1000 ? (ms / 1000).toFixed(1) + 's' : (ms || 0) + 'ms';
  const fmtN = n => (n || 0).toLocaleString();
  const fmtUp = ms => {
    if (!(ms > 0)) return '—';
    const s = Math.floor(ms / 1000), h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60);
    return (h ? h + 'g ' : '') + (h || m ? m + 'p ' : '') + (h ? '' : (s % 60) + 's');
  };
  const PCOLS = ['#1f5fd0', '#c2410c', 'var(--on)', 'var(--warn)', 'var(--dim)'];
  const C = { req: '#1f5fd0', in: '#0e7490', out: '#c2410c' };
  const MCOLS = ['#1f5fd0', '#c2410c', '#12734a', '#9a6700', '#86198f', '#0e7490', '#6d28d9', '#b42318'];
  const LCOLS = [
    ['time', 'time', 'when the gateway got the request'],
    ['model', 'model', 'called model'],
    ['streamed', 'stream', 'whether the request streamed'],
    ['input_tokens', 'in tok', 'input tokens'],
    ['output_tokens', 'out tok', 'output tokens'],
    ['ms', 'latency', 'total upstream response time'],
    ['ttfb_ms', 'TTFB', 'time to first token (stream only)'],
    ['error', 'status', 'ok = upstream 2xx; error = >=400 or dropped connection'],
  ];
  let models = [], series = [], modelSeries = {}, events = [], startedAt = 0;
  let chartDefs = [], chartData = [];

  function provCell(m) {
    m = m || '';
    const i = m.indexOf('/');
    if (i < 1) return esc(m);
    const p = m.slice(0, i);
    let h = 0;
    for (let j = 0; j < p.length; j++) h = (h * 31 + p.charCodeAt(j)) >>> 0;
    const c = PCOLS[h % PCOLS.length];
    return '<span class="pb" style="color:' + c + ';border-color:' + c + '">' + esc(p) + '</span>' + esc(m.slice(i + 1));
  }

  function spark(vals, color) {
    const v = vals.slice(-12), max = Math.max(1, ...v);
    const W = 96, H = 26;
    const x = i => 1 + i * (W - 2) / Math.max(1, v.length - 1);
    const y = t => H - 2 - t * (H - 4) / max;
    const pts = v.map((t, i) => x(i).toFixed(1) + ',' + y(t).toFixed(1)).join(' ');
    return '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H + '" style="display:block">' +
      '<polyline points="' + pts + '" fill="none" stroke="' + color + '" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>' +
      '<circle cx="' + x(v.length - 1).toFixed(1) + '" cy="' + y(v[v.length - 1]).toFixed(1) + '" r="4" fill="' + color + '" style="stroke:var(--panel)" stroke-width="2"/>' +
      '</svg>';
  }

  function lineChart(defs, data) {
    const W = 640, H = 150, PADL = 38, PADR = 10, PADT = 8, PADB = 20;
    if (!data) data = series;
    if (!data.length) data = [{ minute: 0 }];
    const all = defs.map(d => data.map(p => p[d.key] || 0));
    const max = Math.max(1, ...all.flat());
    const tick = max > 4 ? Math.ceil(max / 4) : 1;
    const yMax = Math.max(tick, Math.ceil(max / tick) * tick);
    const x = i => PADL + i * (W - PADL - PADR) / Math.max(1, data.length - 1);
    const y = v => H - PADB - v * (H - PADB - PADT) / yMax;
    let grid = '';
    for (let t = 0; t <= 4; t++) {
      const v = yMax * t / 4, yy = y(v).toFixed(1);
      grid += `<line x1="${PADL}" x2="${W - PADR}" y1="${yy}" y2="${yy}" stroke="#e6e9ee" stroke-width="1"/>` +
        `<text x="${PADL - 6}" y="${+yy + 4}" text-anchor="end" class="ax">${fmtN(Math.round(v))}</text>`;
    }
    let paths = '', legend = '';
    defs.forEach(d => {
      const vals = data.map(p => p[d.key] || 0);
      const pts = vals.map((v, i) => x(i).toFixed(1) + ',' + y(v).toFixed(1)).join(' ');
      paths += `<path d="M${x(0).toFixed(1)},${y(vals[0] || 0).toFixed(1)} L${pts.replace(/ /g, ' L')}" fill="none" stroke="${d.color}" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`;
      paths += `<circle cx="${x(vals.length - 1).toFixed(1)}" cy="${y(vals[vals.length - 1] || 0).toFixed(1)}" r="4" fill="${d.color}" style="stroke:var(--panel)" stroke-width="2"/>`;
      legend += `<span class="lg"><svg width="14" height="8"><line x1="0" y1="4" x2="14" y2="4" stroke="${d.color}" stroke-width="2" stroke-linecap="round"/></svg>${esc(d.label)}</span>`;
    });
    chartDefs.push(defs);
    chartData.push(data);
    return `<div class="chartbox"><div class="chhead">${legend}</div><div class="chwrap" data-chart>` +
      `<svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" style="width:100%;height:150px;display:block">${grid}${paths}` +
      `<line class="crosshair" y1="${PADT}" y2="${H - PADB}" x1="-99" x2="-99" stroke="#9aa6b2" stroke-width="1"/></svg>` +
      `<div class="tip hidden"></div></div></div>`;
  }

  function sortHeader(cols, key, dir) {
    return '<tr>' + cols.map(c => '<th data-lkey="' + c[0] + '"' +
      (c[2] ? ' title="' + esc(c[2]) + '"' : '') +
      (c[0] === key ? ' class="on' + (dir > 0 ? ' asc' : '') + '"' : '') + '>' + c[1] + '</th>').join('') + '</tr>';
  }

  function drawCharts() {
    chartDefs = [];
    chartData = [];
    const tot = k => models.reduce((s, m) => s + (m[k] || 0), 0);
    const last = series[series.length - 1] || {};
    const reqS = series.map(p => p.requests || 0);
    const errPct = tot('requests') ? Math.round(100 * tot('errors') / tot('requests')) : 0;
    const ttfbAvg = tot('streamed')
      ? Math.round(models.reduce((s, m) => s + (m.ttfb_avg_ms || 0) * (m.streamed || 0), 0) / tot('streamed')) : 0;
    const mss = events.map(e => e.ms || 0).sort((a, b) => a - b);
    const p95 = mss.length ? mss[Math.max(0, Math.ceil(0.95 * mss.length) - 1)] : 0;
    const tiles = [
      [fmtN(tot('requests')), 'total requests', reqS, C.req],
      [fmtN(last.requests || 0), 'req / min', reqS, C.req],
      [fmtN(tot('input_tokens')), 'in tokens', series.map(p => p.input_tokens || 0), C.in],
      [fmtN(tot('output_tokens')), 'out tokens', series.map(p => p.output_tokens || 0), C.out],
      [fmtN(tot('errors')) + ' (' + errPct + '%)', 'errors', series.map(p => p.errors || 0), '#c0392b'],
      [ttfbAvg ? fmtMS(ttfbAvg) : '—', 'avg TTFB (stream)', null, null],
      [mss.length ? fmtMS(p95) : '—', 'p95 latency', null, null],
      [fmtUp(startedAt > 0 ? Date.now() - startedAt : 0), 'uptime', null, null],
    ];
    charts.innerHTML =
      '<div class="tiles">' + tiles.map(t =>
        '<div class="tile"><div class="lbl">' + t[1] + '</div><div class="big">' + t[0] + '</div>' +
        (t[2] ? spark(t[2], t[3]) : '') + '</div>').join('') + '</div>' +
      '<div class="note">Covers every API the gateway serves — NVIDIA playground models and ' +
      'passthrough providers alike. Counting since ' +
      (startedAt ? new Date(startedAt).toLocaleString() : 'server start') + '.</div>' +
      '<div class="chartrow">' +
      lineChart([{ key: 'requests', label: 'requests / min', color: C.req }]) +
      lineChart([{ key: 'input_tokens', label: 'in tok / min', color: C.in }, { key: 'output_tokens', label: 'out tok / min', color: C.out }]) +
      lineChart([{ key: 'ttfb_avg_ms', label: 'avg TTFB / min', color: C.req }]) +
      '</div>' +
      '<div class="note">TTFB = time to first byte of the stream. Minutes with no streamed requests show 0.</div>' +
      '<div class="note"><b>Per-model requests / min (top 8)</b></div>' +
      (Object.keys(modelSeries).length
        ? (() => {
            const top = Object.entries(modelSeries)
              .map(([m, s]) => [m, s.reduce((a, p) => a + (p.requests || 0), 0)])
              .sort((a, b) => b[1] - a[1]).slice(0, 8).map(x => x[0]);
            const mcol = m => { let h = 0; for (let j = 0; j < m.length; j++) h = (h * 31 + m.charCodeAt(j)) >>> 0; return MCOLS[h % MCOLS.length]; };
            const grid = series.map(p => {
              const o = { minute: p.minute };
              top.forEach((m, k) => { o['m' + k] = (modelSeries[m] && modelSeries[m][series.indexOf(p)] && modelSeries[m][series.indexOf(p)].requests) || 0; });
              return o;
            });
            return lineChart(top.map((m, k) => ({ key: 'm' + k, label: m, color: mcol(m) })), grid);
          })()
        : emptyRow('No requests yet.'));
    armCharts();
  }

  function armCharts() {
    charts.querySelectorAll('.chwrap').forEach((w, ci) => {
      const svg = w.querySelector('svg'), cross = svg.querySelector('.crosshair'), tip = w.querySelector('.tip');
      const defs = chartDefs[ci] || [];
      const move = e => {
        const r = svg.getBoundingClientRect();
        const px = (e.clientX - r.left) / r.width * 640;
        const i = Math.max(0, Math.min(series.length - 1, Math.round((px - 38) / ((640 - 38 - 10) / Math.max(1, series.length - 1)))));
        const vx = 38 + i * (640 - 38 - 10) / Math.max(1, series.length - 1);
        cross.setAttribute('x1', vx);
        cross.setAttribute('x2', vx);
        const p = (chartData[ci] || series)[i];
        if (!p) return;
        tip.textContent = '';
        const head = document.createElement('div');
        head.className = 'tip-h';
        head.textContent = p.minute ? new Date(p.minute).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';
        tip.appendChild(head);
        defs.forEach(d => {
          const row = document.createElement('div');
          row.className = 'tip-r';
          const key = document.createElement('span');
          key.className = 'tip-k';
          const sw = document.createElement('span');
          sw.className = 'tip-sw';
          sw.style.background = d.color;
          key.appendChild(sw);
          key.appendChild(document.createTextNode(d.label));
          const val = document.createElement('b');
          val.textContent = (d.fmt || fmtN)(p[d.key] || 0);
          row.appendChild(key);
          row.appendChild(val);
          tip.appendChild(row);
        });
        tip.classList.remove('hidden');
        const tx = Math.min(r.width - 130, Math.max(4, e.clientX - r.left + 12));
        tip.style.left = tx + 'px';
        tip.style.top = '6px';
      };
      w.addEventListener('pointermove', move);
      w.addEventListener('pointerleave', () => {
        cross.setAttribute('x1', -99);
        cross.setAttribute('x2', -99);
        tip.classList.add('hidden');
      });
    });
  }

  function logTable(evs, key, dir) {
    const lval = (e, k) => k === 'time' ? +new Date(e.time)
      : k === 'model' ? (e.model || '')
      : typeof e[k] === 'boolean' ? (e[k] ? 1 : 0) : (e[k] || 0);
    const sorted = evs.slice().sort((a, b) => {
      const va = lval(a, key), vb = lval(b, key);
      return (typeof va === 'string' ? va.localeCompare(vb) : va - vb) * dir;
    });
    return '<div class="logwrap"><table class="stats"><thead>' + sortHeader(LCOLS, key, dir) + '</thead><tbody>' +
      (sorted.length ? sorted.map(e => '<tr>' +
        '<td>' + fmtTime(e.time) + '</td>' +
        '<td class="nm">' + provCell(e.model) + '</td>' +
        '<td>' + (e.streamed ? 'yes' : '—') + '</td>' +
        '<td>' + fmtN(e.input_tokens) + '</td>' +
        '<td>' + fmtN(e.output_tokens) + '</td>' +
        '<td>' + fmtMS(e.ms) + '</td>' +
        '<td>' + (e.ttfb_ms ? fmtMS(e.ttfb_ms) : '—') + '</td>' +
        '<td>' + (e.error ? '<span class="badge">error</span>' : '<span class="badge ok">ok</span>') + '</td>' +
        '</tr>').join('') : '<tr><td colspan="8" class="nm">No requests yet.</td></tr>') +
      '</tbody></table></div>';
  }

  let totalFrames = 0, curFrame = 0, fKey = 'time', fDir = -1, fEvents = [];
  let frameReadAt = 0, frameTimer, frameLoading = false;

  function drawFrames() {
    if (!totalFrames) { framesBox.innerHTML = ''; return; }
    let lo = 1, hi = totalFrames, nav = '';
    if (totalFrames > 12) {
      lo = Math.floor((curFrame - 1) / 12) * 12 + 1;
      hi = Math.min(totalFrames, lo + 11);
      nav = '<button type="button" data-f="prev">«</button>';
    }
    let chips = '';
    for (let f = lo; f <= hi; f++) chips += '<button type="button" class="fp' + (f === curFrame ? ' on' : '') + '" data-f="' + f + '">' + f + '</button>';
    framesBox.innerHTML = nav + chips + (totalFrames > 12 ? '<button type="button" data-f="next">»</button>' : '');
  }

  async function openFrame(n) {
    if (gen !== renderGen) return;
    n = Math.max(1, Math.min(totalFrames, n));
    curFrame = n;
    drawFrames();
    clearTimeout(frameTimer);
    const wait = frameReadAt + INTERVAL - Date.now();
    if (wait > 0 || frameLoading) {
      frameTimer = setTimeout(() => openFrame(n), Math.max(wait, INTERVAL));
      return;
    }
    frameReadAt = Date.now();
    try {
      frameLoading = true;
      const res = await fetch('/admin/api/stats?frames=' + n, { credentials: 'same-origin' });
      if (res.status === 401) { location.href = '/admin/login'; return; }
      if (res.status === 429) {
        frameTimer = setTimeout(() => openFrame(curFrame), Math.max(INTERVAL, (+res.headers.get('Retry-After') || 1) * 1000));
        return;
      }
      const d = await res.json().catch(() => ({}));
      if (n !== curFrame || gen !== renderGen) return;
      if (!res.ok) { frameBox.textContent = 'frame error: ' + (d.error || res.status); return; }
      totalFrames = d.total_frames || totalFrames;
      drawFrames();
      fEvents = d.events || [];
      frameBox.innerHTML = logTable(fEvents, fKey, fDir);
    } catch (err) {
      frameBox.textContent = 'frame error: ' + err.message;
    } finally {
      frameLoading = false;
    }
  }

  framesBox.addEventListener('click', e => {
    const b = e.target.closest('button[data-f]');
    if (!b) return;
    const f = b.dataset.f;
    openFrame(f === 'prev' ? curFrame - 12 : f === 'next' ? curFrame + 12 : +f);
  });
  frameBox.addEventListener('click', e => {
    const th = e.target.closest('th[data-lkey]');
    if (!th) return;
    if (fKey === th.dataset.lkey) fDir = -fDir;
    else { fKey = th.dataset.lkey; fDir = -1; }
    frameBox.innerHTML = logTable(fEvents, fKey, fDir);
  });

  async function poll() {
    if (gen !== renderGen) return;
    try {
      const d = await api('/admin/api/stats');
      models = d.models || [];
      series = d.series || [];
      modelSeries = d.series_by_model || {};
      events = d.events || [];
      startedAt = d.started_at ? +new Date(d.started_at) : 0;
      drawCharts();
      totalFrames = d.total_frames || 0;
      if (!curFrame && totalFrames) await openFrame(1);
      else drawFrames();
    } catch (err) {
      if (gen === renderGen) charts.textContent = 'stats error: ' + err.message;
    }
    if (gen === renderGen) setTimeout(poll, INTERVAL);
  }
  poll();
}

// --- models & providers --------------------------------------------------

const PROVIDER_EDITOR = `<details id="editor-panel" class="sheet">
<summary class="sheet-head"><span class="sheet-title" id="editor-heading">Add upstream provider</span><span class="sub">Aliases, headers, timeouts</span></summary>
<form id="provider-editor" class="settings-card">
<input type="hidden" id="p-edit-id" value="">
<div class="form-grid">
  <div><label for="p-id">Provider ID</label><input id="p-id" placeholder="e.g. openrouter" required></div>
  <div><label for="p-name">Provider Name</label><input id="p-name" placeholder="e.g. OpenRouter" required></div>
  <div><label for="p-base-url">Base URL</label><input id="p-base-url" placeholder="https://openrouter.ai/api/v1" required></div>
  <div><label for="p-protocol">Protocol</label><select id="p-protocol"><option value="openai-chat">OpenAI Chat (openai-chat)</option><option value="anthropic-messages">Anthropic Messages (anthropic-messages)</option><option value="openai-responses">OpenAI Responses (openai-responses)</option></select></div>
  <div><label for="p-auth-scheme">Auth Scheme</label><select id="p-auth-scheme"><option value="bearer">Bearer</option><option value="x-api-key">API Key (x-api-key)</option><option value="none">None</option></select></div>
  <div><label for="p-auth-key">API Key <span class="dim" id="p-auth-key-hint">(password/write-only)</span></label><input id="p-auth-key" type="password" placeholder="API key (leave blank to keep existing)"></div>
  <div><label for="p-auth-key-env">KeyEnv (Environment Variable)</label><input id="p-auth-key-env" placeholder="e.g. OPENROUTER_API_KEY"></div>
</div>
<div style="margin-top:.75rem"><div style="display:flex;justify-content:space-between;align-items:center"><label style="font-weight:600;color:var(--fg);font-size:.82rem">Model Mappings</label><button type="button" id="add-model-btn" class="btn-sm">+ Add Model</button></div><div class="sub">Map gateway model alias to upstream model ID. Configure hide/show toggles and fallback chains.</div><div id="model-mappings-list" style="margin-top:.35rem"></div></div>
<div style="margin-top:.75rem"><div style="display:flex;justify-content:space-between;align-items:center"><label style="font-weight:600;color:var(--fg);font-size:.82rem">Custom Headers</label><button type="button" id="add-header-btn" class="btn-sm">+ Add Header</button></div><div class="sub">Custom HTTP headers forwarded to the upstream endpoint.</div><div id="custom-headers-list" style="margin-top:.35rem"></div></div>
<details id="p-advanced" style="margin-top:.75rem"><summary style="cursor:pointer;color:var(--dim);font-size:.82rem;font-weight:600">Advanced Settings (Timeouts, Concurrency, TLS)</summary><div style="margin-top:.5rem;padding-left:.5rem;border-left:2px solid var(--line)"><div class="form-grid">
  <div><label for="p-max-concurrency">Max Concurrency</label><input type="number" id="p-max-concurrency" min="0" placeholder="0 (unlimited)"></div>
  <div><label for="p-timeout-connect">Connect Timeout (seconds)</label><input type="number" step="any" min="0" id="p-timeout-connect" placeholder="e.g. 10"></div>
  <div><label for="p-timeout-queue">Queue Timeout (seconds)</label><input type="number" step="any" min="0" id="p-timeout-queue" placeholder="e.g. 5"></div>
  <div><label for="p-timeout-idle">Idle Timeout (seconds)</label><input type="number" step="any" min="0" id="p-timeout-idle" placeholder="e.g. 30"></div>
  <div><label for="p-tls">TLS Profile</label><select id="p-tls"><option value="default">Default</option><option value="chrome">Chrome (TLS impersonation)</option></select></div>
</div></div></details>
<div class="bar" style="margin-top:.9rem"><button type="submit" id="save-provider-btn" class="primary">Save Provider</button><button type="button" id="test-editor-btn">Test Connection</button><button type="button" id="cancel-editor-btn" style="display:none">Cancel</button><span id="editor-test-badge" class="test-res"></span></div>
</form></details>`;

function providerCard(p) {
  const aliases = (p.models || []).map(m =>
    `<span class="badge ${m.hidden ? 'warn' : ''}" title="Upstream: ${esc(m.upstream_model)}">${esc(m.alias)}${m.hidden ? ' (hidden)' : ''}</span>`
  ).join('') || '<span class="dim" style="font-size:.75rem">none</span>';
  return `<div class="prov-card" data-provider-id="${esc(p.id)}">
    <div class="prov-head"><span class="badge pb ok">${esc(p.protocol)}</span><strong>${esc(p.name)}</strong><span class="chip">${esc(p.id)}</span><span class="prov-meta">${esc(p.base_url)}</span><input type="checkbox" class="sw prov-toggle" data-id="${esc(p.id)}"${p.enabled ? ' checked' : ''} title="Toggle enabled"></div>
    <div class="prov-aliases"><span class="alias-lbl">Aliases</span>${aliases}</div>
    <div class="prov-actions"><button type="button" class="btn-sm" data-edit="${esc(p.id)}">Edit</button><button type="button" class="btn-sm" data-test="${esc(p.id)}">Test Connection</button><button type="button" class="btn-sm btn-del" data-delete="${esc(p.id)}">Delete</button><span class="test-res"></span></div>
  </div>`;
}

function catalogHTML(list) {
  if (!list.length) return emptyRow('No models in the catalog.');
  const counts = {};
  list.forEach(m => { const p = m.publisher || 'custom'; counts[p] = (counts[p] || 0) + 1; });
  let group, out = '';
  list.forEach(m => {
    const pub = m.publisher || 'custom';
    if (pub !== group) {
      group = pub;
      out += `<div class="grp" data-grp>${esc(pub)}<span class="cnt">${counts[pub]}</span></div>`;
    }
    out += `<div class="row" data-id="${esc(m.id)}" data-search="${esc((m.name + ' ' + m.id).toLowerCase())}">
      <input type="checkbox" class="sw" value="${esc(m.id)}"${m.hidden ? '' : ' checked'}>
      <span class="name"${m.base_url ? ` title="${esc(m.base_url)}"` : ''}>${esc(m.name)}</span>
      <span class="id">${esc(m.id)}</span>
      <button type="button" class="del-btn" title="Delete model" data-del="${esc(m.id)}">×</button>
    </div>`;
  });
  return out;
}

async function renderModels(gen) {
  const [providers, data] = await Promise.all([api('/admin/api/providers'), api('/admin/api/models')]);
  if (gen !== renderGen) return;
  const models = (data.models || []).slice().sort((a, b) => {
    const pa = a.publisher || 'custom', pb = b.publisher || 'custom';
    return pa === pb ? String(a.name).localeCompare(String(b.name)) : pa.localeCompare(pb);
  });

  view.innerHTML = `<div class="tools">
      <input type="search" id="q" placeholder="Filter by name or model id">
      <button type="button" id="all">Enable all</button>
      <button type="button" id="none">Disable all</button>
      <button type="button" id="retry">Retry failed</button>
      <span class="sub" id="counts"></span>
    </div>
    <div class="split"><div class="pane">
      <section class="sheet"><div class="sheet-head"><h2>Upstream providers</h2><span class="sub">${providers.length} configured</span><button type="button" id="add-provider-btn" class="btn-sm">Add provider</button></div><div id="provider-list">${providers.length ? providers.map(providerCard).join('') : emptyRow('No upstream providers configured yet. Add one to route gateway models to an upstream API.')}</div></section>
      ${PROVIDER_EDITOR}
    </div><div class="pane">
      <section class="sheet" id="catalog"><div class="sheet-head"><h2>Model catalog</h2><span class="sub">${models.length} models · grouped by publisher</span></div>${catalogHTML(models)}</section>
    </div></div>`;

  const list = document.getElementById('provider-list');
  const catalog = document.getElementById('catalog');
  const counts = document.getElementById('counts');
  const editor = document.getElementById('provider-editor');
  let pendingMasks = Promise.resolve();

  const renderCounts = () => {
    const all = catalog.querySelectorAll('.row[data-id] .sw');
    const on = [...all].filter(s => s.checked).length;
    counts.textContent = on + ' on · ' + (all.length - on) + ' off';
  };

  function addModelRow(alias = '', upstream = '', hidden = false, fallbacks = '') {
    const box = document.getElementById('model-mappings-list');
    const row = document.createElement('div');
    row.className = 'mm-row';
    row.innerHTML = '<input class="mm-alias" placeholder="Alias (e.g. gpt-4o)">' +
      '<input class="mm-upstream" placeholder="Upstream model (e.g. openai/gpt-4o)">' +
      '<label title="Hide from client catalog"><input type="checkbox" class="mm-hidden"> Hide</label>' +
      '<input class="mm-fallbacks" placeholder="Fallbacks (comma-separated)">' +
      '<button type="button" class="del-row-btn" title="Remove model mapping">×</button>';
    row.querySelector('.mm-alias').value = alias;
    row.querySelector('.mm-upstream').value = upstream;
    row.querySelector('.mm-hidden').checked = hidden;
    row.querySelector('.mm-fallbacks').value = fallbacks;
    row.querySelector('.del-row-btn').addEventListener('click', () => row.remove());
    box.appendChild(row);
  }

  function addHeaderRow(name = '', value = '') {
    const box = document.getElementById('custom-headers-list');
    const row = document.createElement('div');
    row.className = 'hdr-row';
    row.innerHTML = '<input class="hdr-name" placeholder="Header name (e.g. HTTP-Referer)">' +
      '<input class="hdr-val" placeholder="Header value">' +
      '<button type="button" class="del-row-btn" title="Remove header">×</button>';
    row.querySelector('.hdr-name').value = name;
    row.querySelector('.hdr-val').value = value;
    row.querySelector('.del-row-btn').addEventListener('click', () => row.remove());
    box.appendChild(row);
  }

  const nsToSec = ns => (ns && ns > 0) ? ns / 1e9 : '';
  const secToNs = sec => {
    const n = parseFloat(sec);
    return (isNaN(n) || n <= 0) ? 0 : Math.round(n * 1e9);
  };

  function resetEditor() {
    document.getElementById('p-edit-id').value = '';
    document.getElementById('editor-heading').textContent = 'Add upstream provider';
    const id = document.getElementById('p-id');
    id.value = '';
    id.readOnly = false;
    document.getElementById('p-name').value = '';
    document.getElementById('p-base-url').value = '';
    document.getElementById('p-protocol').value = 'openai-chat';
    document.getElementById('p-auth-scheme').value = 'bearer';
    document.getElementById('p-auth-key').value = '';
    document.getElementById('p-auth-key').disabled = false;
    document.getElementById('p-auth-key-hint').textContent = '(password/write-only)';
    document.getElementById('p-auth-key-env').value = '';
    document.getElementById('p-auth-key-env').disabled = false;
    document.getElementById('p-max-concurrency').value = '';
    document.getElementById('p-timeout-connect').value = '';
    document.getElementById('p-timeout-queue').value = '';
    document.getElementById('p-timeout-idle').value = '';
    document.getElementById('p-tls').value = 'default';
    document.getElementById('model-mappings-list').innerHTML = '';
    document.getElementById('custom-headers-list').innerHTML = '';
    document.getElementById('cancel-editor-btn').style.display = 'none';
    document.getElementById('editor-test-badge').innerHTML = '';
    addModelRow();
  }

  function loadEditor(id) {
    const p = providers.find(x => x.id === id);
    const panel = document.getElementById('editor-panel');
    if (!p || !panel) return;
    document.getElementById('p-edit-id').value = p.id;
    document.getElementById('editor-heading').textContent = 'Edit upstream provider (' + p.id + ')';
    const idInput = document.getElementById('p-id');
    idInput.value = p.id;
    idInput.readOnly = true;
    document.getElementById('p-name').value = p.name || '';
    document.getElementById('p-base-url').value = p.base_url || '';
    document.getElementById('p-protocol').value = p.protocol || 'openai-chat';
    let scheme = (p.auth && p.auth.scheme) || 'bearer';
    if (scheme === 'api-key') scheme = 'x-api-key';
    document.getElementById('p-auth-scheme').value = scheme;
    const key = document.getElementById('p-auth-key');
    const keyEnv = document.getElementById('p-auth-key-env');
    const hint = document.getElementById('p-auth-key-hint');
    key.value = '';
    if (scheme === 'none') {
      key.disabled = keyEnv.disabled = true;
      hint.textContent = '(disabled for none)';
    } else {
      key.disabled = keyEnv.disabled = false;
      const hasKey = p.auth && (p.auth.key || p.auth.key_ref);
      hint.textContent = hasKey ? '(current: ' + (p.auth.key || '[REDACTED]') + ' - leave blank to keep)' : '(password/write-only)';
    }
    keyEnv.value = (p.auth && p.auth.key_env) || '';
    const mm = document.getElementById('model-mappings-list');
    mm.innerHTML = '';
    if (p.models && p.models.length) {
      p.models.forEach(m => addModelRow(m.alias, m.upstream_model, !!m.hidden, (m.fallbacks || []).join(', ')));
    } else {
      addModelRow();
    }
    const hdr = document.getElementById('custom-headers-list');
    hdr.innerHTML = '';
    Object.keys(p.headers || {}).forEach(k => addHeaderRow(k, p.headers[k]));
    document.getElementById('p-max-concurrency').value = p.max_concurrency || '';
    document.getElementById('p-timeout-connect').value = nsToSec(p.timeouts && p.timeouts.connect);
    document.getElementById('p-timeout-queue').value = nsToSec(p.queue_timeout);
    document.getElementById('p-timeout-idle').value = nsToSec(p.timeouts && p.timeouts.idle);
    document.getElementById('p-tls').value = p.tls || 'default';
    document.getElementById('cancel-editor-btn').style.display = 'inline-block';
    document.getElementById('editor-test-badge').innerHTML = '';
    panel.open = true;
    panel.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  function collectProvider() {
    const editId = document.getElementById('p-edit-id').value.trim();
    const isEdit = !!editId;
    const id = isEdit ? editId : document.getElementById('p-id').value.trim();
    const name = document.getElementById('p-name').value.trim();
    const baseURL = document.getElementById('p-base-url').value.trim();
    const protocol = document.getElementById('p-protocol').value;
    let scheme = document.getElementById('p-auth-scheme').value;
    if (scheme === 'api-key') scheme = 'x-api-key';
    const key = document.getElementById('p-auth-key').value.trim();
    const keyEnv = document.getElementById('p-auth-key-env').value.trim();
    const auth = { scheme };
    if (scheme !== 'none') {
      if (key) auth.key = key;
      if (keyEnv) auth.key_env = keyEnv;
    }
    const existing = isEdit ? providers.find(x => x.id === id) : null;
    if (existing && existing.auth && existing.auth.header_name) auth.header_name = existing.auth.header_name;

    const models = [];
    document.querySelectorAll('#model-mappings-list .mm-row').forEach(r => {
      const alias = r.querySelector('.mm-alias').value.trim();
      if (!alias) return;
      const upstream = r.querySelector('.mm-upstream').value.trim();
      const hidden = r.querySelector('.mm-hidden').checked;
      const fallbacks = r.querySelector('.mm-fallbacks').value.trim()
        .split(',').map(s => s.trim()).filter(Boolean);
      const item = { alias, upstream_model: alias.endsWith('/*') ? upstream : (upstream || alias) };
      if (hidden) item.hidden = true;
      if (fallbacks.length) item.fallbacks = fallbacks;
      models.push(item);
    });

    const headers = {};
    document.querySelectorAll('#custom-headers-list .hdr-row').forEach(r => {
      const hn = r.querySelector('.hdr-name').value.trim();
      if (hn) headers[hn] = r.querySelector('.hdr-val').value;
    });

    const maxConcurrency = parseInt(document.getElementById('p-max-concurrency').value, 10) || 0;
    const connSec = secToNs(document.getElementById('p-timeout-connect').value);
    const queueSec = secToNs(document.getElementById('p-timeout-queue').value);
    const idleSec = secToNs(document.getElementById('p-timeout-idle').value);
    const tls = document.getElementById('p-tls').value || 'default';

    let payload;
    if (existing) {
      payload = JSON.parse(JSON.stringify(existing));
      payload.id = id;
      payload.name = name;
      payload.base_url = baseURL;
      payload.protocol = protocol;
      payload.auth = auth;
      payload.models = models;
      payload.headers = Object.keys(headers).length ? headers : undefined;
      payload.max_concurrency = maxConcurrency;
      payload.queue_timeout = queueSec;
      payload.tls = tls;
      if (!payload.timeouts) payload.timeouts = {};
      if (connSec > 0) payload.timeouts.connect = connSec; else delete payload.timeouts.connect;
      if (idleSec > 0) payload.timeouts.idle = idleSec; else delete payload.timeouts.idle;
      if (!Object.keys(payload.timeouts).length) delete payload.timeouts;
    } else {
      const timeouts = {};
      if (connSec > 0) timeouts.connect = connSec;
      if (idleSec > 0) timeouts.idle = idleSec;
      payload = {
        id, name, base_url: baseURL, protocol, auth, models,
        headers: Object.keys(headers).length ? headers : undefined,
        max_concurrency: maxConcurrency,
        queue_timeout: queueSec,
        tls,
        enabled: true,
      };
      if (Object.keys(timeouts).length) payload.timeouts = timeouts;
    }
    return { isEdit, id, payload };
  }

  function renderTestBadge(el, data) {
    if (!el) return;
    const lat = (data && data.latency_ms != null) ? '<span class="dim">' + data.latency_ms + 'ms</span> ' : '';
    if (data && data.ok) {
      el.innerHTML = '<span class="badge ok">OK</span> ' + lat + '<span style="color:var(--on);font-size:.8rem">' + esc(data.message || 'Connection successful') + '</span>';
    } else {
      el.innerHTML = '<span class="badge">FAIL</span> ' + lat + '<span style="color:var(--bad);font-size:.8rem">' + esc((data && (data.error || data.status)) || 'Connection failed') + '</span>';
    }
  }

  async function saveMasks() {
    await new Promise(r => setTimeout(r, 0));
    const hidden = [...catalog.querySelectorAll('.row[data-id] .sw')].filter(s => !s.checked).map(s => s.value);
    try {
      await api('/admin/api/models', { method: 'POST', body: { masks_id: hidden } });
      toast('saved');
    } catch (err) {
      toast('save failed: ' + err.message);
    }
  }

  // IDs contain "/" (publisher/model), which cannot occupy one gin path
  // segment. Try the JSON API first; fall back to the legacy body-based
  // endpoint so every catalog row stays deletable.
  async function modelAction(id, action) {
    if (action === 'delete') {
      try {
        return await api('/admin/api/models/' + encodeURIComponent(id), { method: 'DELETE' });
      } catch (err) {
        if (!id.includes('/')) throw err;
        return api('/admin/delete', { method: 'POST', body: { id } });
      }
    }
    try {
      return await api('/admin/api/models/' + encodeURIComponent(id) + '/restore', { method: 'POST' });
    } catch (err) {
      if (!id.includes('/')) throw err;
      return api('/admin/delete', { method: 'POST', body: { id, restore: true } });
    }
  }

  list.addEventListener('click', async e => {
    const btn = e.target.closest('button');
    if (!btn) return;
    if (btn.dataset.edit) { loadEditor(btn.dataset.edit); return; }
    if (btn.dataset.test) {
      const card = btn.closest('.prov-card');
      const badgeEl = card && card.querySelector('.test-res');
      if (badgeEl) badgeEl.innerHTML = '<span class="dim">Testing…</span>';
      try {
        const data = await api('/admin/api/providers/' + encodeURIComponent(btn.dataset.test) + '/test', { method: 'POST', body: {} });
        renderTestBadge(badgeEl, data);
      } catch (err) {
        renderTestBadge(badgeEl, { error: err.message });
      }
      return;
    }
    if (btn.dataset.delete) {
      if (!confirm('Delete provider "' + btn.dataset.delete + '"?')) return;
      try {
        await api('/admin/api/providers/' + encodeURIComponent(btn.dataset.delete), { method: 'DELETE' });
        toast('deleted provider ' + btn.dataset.delete);
        route();
      } catch (err) {
        toast('delete failed: ' + err.message);
      }
    }
  });

  list.addEventListener('change', async e => {
    if (!e.target.classList.contains('prov-toggle')) return;
    const id = e.target.dataset.id;
    const p = providers.find(x => x.id === id);
    if (!p) return;
    const enabled = e.target.checked;
    try {
      await api('/admin/api/providers/' + encodeURIComponent(id), { method: 'PUT', body: { ...p, enabled } });
      p.enabled = enabled;
      toast((enabled ? 'enabled ' : 'disabled ') + id);
    } catch (err) {
      e.target.checked = !enabled;
      toast('toggle failed: ' + err.message);
    }
  });

  document.getElementById('add-provider-btn').addEventListener('click', () => {
    resetEditor();
    const panel = document.getElementById('editor-panel');
    panel.open = true;
    panel.scrollIntoView({ behavior: 'smooth', block: 'start' });
    document.getElementById('p-id').focus();
  });
  document.getElementById('cancel-editor-btn').addEventListener('click', resetEditor);
  document.getElementById('add-model-btn').addEventListener('click', () => addModelRow());
  document.getElementById('add-header-btn').addEventListener('click', () => addHeaderRow());
  document.getElementById('p-auth-scheme').addEventListener('change', e => {
    const scheme = e.target.value;
    const key = document.getElementById('p-auth-key');
    const keyEnv = document.getElementById('p-auth-key-env');
    const hint = document.getElementById('p-auth-key-hint');
    if (scheme === 'none') {
      key.disabled = keyEnv.disabled = true;
      key.value = keyEnv.value = '';
      hint.textContent = '(disabled for none)';
    } else {
      key.disabled = keyEnv.disabled = false;
      hint.textContent = document.getElementById('p-edit-id').value ? '(leave blank to keep existing key)' : '(password/write-only)';
    }
  });

  editor.addEventListener('submit', async e => {
    e.preventDefault();
    const { isEdit, id, payload } = collectProvider();
    if (!id || !payload.name || !payload.base_url) {
      toast('Provider ID, name and base URL are required');
      return;
    }
    try {
      await api(isEdit ? '/admin/api/providers/' + encodeURIComponent(id) : '/admin/api/providers', {
        method: isEdit ? 'PUT' : 'POST', body: payload,
      });
      toast((isEdit ? 'updated ' : 'created ') + id);
      route();
    } catch (err) {
      toast('save failed: ' + err.message);
    }
  });

  document.getElementById('test-editor-btn').addEventListener('click', async () => {
    const { isEdit, id, payload } = collectProvider();
    if (!payload.base_url) { toast('Base URL is required to test'); return; }
    const out = document.getElementById('editor-test-badge');
    out.innerHTML = '<span class="dim">Testing…</span>';
    try {
      const url = isEdit ? '/admin/api/providers/' + encodeURIComponent(id) + '/test' : '/admin/api/providers/test';
      renderTestBadge(out, await api(url, { method: 'POST', body: payload }));
    } catch (err) {
      renderTestBadge(out, { error: err.message });
    }
  });

  catalog.addEventListener('change', e => {
    if (!e.target.classList.contains('sw')) return;
    renderCounts();
    pendingMasks = pendingMasks.catch(() => {}).then(saveMasks);
  });
  catalog.addEventListener('click', async e => {
    const del = e.target.closest('[data-del]');
    if (!del) return;
    e.stopPropagation();
    if (!confirm('Delete model "' + del.dataset.del + '"?')) return;
    try {
      await modelAction(del.dataset.del, 'delete');
      toast('deleted ' + del.dataset.del);
      route();
    } catch (err) {
      toast('delete failed: ' + err.message);
    }
  });

  document.getElementById('q').addEventListener('input', e => {
    const q = e.target.value.toLowerCase();
    catalog.querySelectorAll('.row[data-id]').forEach(r => {
      r.classList.toggle('hide', !r.dataset.search.includes(q));
    });
    catalog.querySelectorAll('.grp').forEach(g => {
      let visible = false, n = g.nextElementSibling;
      while (n && !n.classList.contains('grp')) {
        if (n.classList.contains('row') && !n.classList.contains('hide')) { visible = true; break; }
        n = n.nextElementSibling;
      }
      g.classList.toggle('hide', !visible);
    });
  });
  document.getElementById('all').addEventListener('click', () => {
    catalog.querySelectorAll('.row[data-id] .sw').forEach(s => { s.checked = true; });
    renderCounts();
    pendingMasks = pendingMasks.catch(() => {}).then(saveMasks);
  });
  document.getElementById('none').addEventListener('click', () => {
    catalog.querySelectorAll('.row[data-id] .sw').forEach(s => { s.checked = false; });
    renderCounts();
    pendingMasks = pendingMasks.catch(() => {}).then(saveMasks);
  });
  document.getElementById('retry').addEventListener('click', async () => {
    try {
      await api('/admin/api/models/retry-failed', { method: 'POST' });
      toast('retry queued');
      route();
    } catch (err) {
      toast('retry failed: ' + err.message);
    }
  });

  renderCounts();
  addModelRow();
  const deleted = data.deleted || models.filter(m => m.deleted).map(m => m.id);
  if (deleted.length) {
    const sec = document.createElement('div');
    sec.id = 'deleted-section';
    sec.className = 'collapsed';
    sec.innerHTML = `<div id="deleted-header"><span class="arrow">▶</span> <span>Deleted models</span> <span class="cnt">${deleted.length} model${deleted.length === 1 ? '' : 's'}</span></div>
      <div id="deleted-list">${deleted.map(id => `<div class="del-row"><span class="id">${esc(id)}</span><button type="button" data-restore="${esc(id)}">restore</button></div>`).join('')}</div>`;
    view.appendChild(sec);
    document.getElementById('deleted-header').addEventListener('click', () => sec.classList.toggle('collapsed'));
    sec.addEventListener('click', async e => {
      const b = e.target.closest('[data-restore]');
      if (!b) return;
      try {
        await modelAction(b.dataset.restore, 'restore');
        toast('restored ' + b.dataset.restore);
        route();
      } catch (err) {
        toast('restore failed: ' + err.message);
      }
    });
  }
}

// --- config --------------------------------------------------------------

const SEARCH_ENGINES = [
  ['', 'Disabled'],
  ['https://www.google.com/search?q={query}', 'Google'],
  ['https://scholar.google.com/scholar?q={query}', 'Google Scholar'],
  ['https://www.bing.com/search?q={query}', 'Bing'],
  ['https://www.duckduckgo.com/html/?q={query}', 'DuckDuckGo'],
  ['custom', 'Custom URL'],
];

async function renderConfig(gen) {
  const [data, auto, cycle, search] = await Promise.all([
    api('/admin/api/models'),
    api('/admin/api/config/automode'),
    api('/admin/api/config/cycle'),
    api('/admin/api/config/search'),
  ]);
  if (gen !== renderGen) return;

  const visible = (data.models || []).filter(m => !m.hidden).map(m => m.id);
  if (!visible.includes('cycle')) visible.push('cycle');
  const autoSelected = auto.model || '';
  if (autoSelected && !visible.includes(autoSelected)) visible.push(autoSelected);
  const autoOptions = visible.map(id =>
    `<option value="${esc(id)}"${id === autoSelected ? ' selected' : ''}>${esc(id)}</option>`).join('');

  const searchURL = search.url || '';
  let searchChoice = '';
  if (searchURL) {
    searchChoice = 'custom';
    for (const [value, label] of SEARCH_ENGINES) {
      if (value && value !== 'custom' && value === searchURL) { searchChoice = value; break; }
    }
  }
  const engineOptions = SEARCH_ENGINES.map(([value, label]) =>
    `<option value="${esc(value)}"${value === searchChoice ? ' selected' : ''}>${esc(label)}</option>`).join('');

  const selected = new Set(cycle.models || []);
  const available = (cycle.available && cycle.available.length)
    ? cycle.available
    : [...new Set([...(cycle.models || []), ...(data.models || []).filter(m => !m.hidden).map(m => m.id)])];
  const ordered = [...(cycle.models || []).filter(id => available.includes(id))];
  available.forEach(id => { if (!selected.has(id)) ordered.push(id); });
  const cycleRows = ordered.length ? ordered.map((id, i) =>
    `<div class="cycle-option"><span class="pos">${i + 1}</span><label><input type="checkbox" value="${esc(id)}"${selected.has(id) ? ' checked' : ''}> ${esc(id)}</label><span><button type="button" data-move="up" title="Move up">↑</button><button type="button" data-move="down" title="Move down">↓</button></span></div>`
  ).join('') : emptyRow('No models available for the cycle.');

  view.innerHTML = `<div class="cols">
    <section class="sheet"><div class="sheet-head"><h2>Auto model</h2></div><div class="settings-card">
      <p>Send <code>model: "automode"</code> and the gateway forwards the request, unchanged, to the model picked here.</p>
      <label for="auto-model">Model requests to <code>automode</code> go to</label>
      <select id="auto-model">${autoOptions}</select>
      <p>Current target: <code>${esc(autoSelected)}</code></p>
    </div></section>
    <section class="sheet"><div class="sheet-head"><h2>Web search</h2></div><form id="search-form" class="settings-card">
      <p>Backs the gateway search tool that Claude Code calls. Pick an engine or give a public HTTPS URL; custom sites take <code>q</code> or <code>{query}</code>. Saves immediately and survives restarts.</p>
      <label for="search-provider">Search engine</label><select id="search-provider">${engineOptions}</select>
      <label for="search-url">Search URL</label><input id="search-url" type="text" inputmode="url" placeholder="https://example.com/search?q={query}" value="${esc(searchURL)}"${searchChoice === 'custom' ? '' : ' readonly'}>
      <button type="submit" class="primary">Save search</button>
    </form></section></div>
    <section class="sheet"><div class="sheet-head"><h2>Model cycle</h2><span class="sub">${(cycle.models || []).length} of ${ordered.length} selected</span></div>
      <form id="cycle-form" class="settings-card"><p>The <code>cycle</code> model is always available. Select models in priority order; changes save automatically. A 429, missing model, timeout or upstream server error moves to the next model.</p><label>Models in order</label><div id="cycle-list" class="cycle-list">${cycleRows}</div>
      <p>${cycle.current ? 'Current target: <code>' + esc(cycle.current) + '</code>' : 'Select a model before sending requests to <code>cycle</code>.'}</p>
      <button type="submit" class="primary">Save cycle</button></form></section>`;

  document.getElementById('auto-model').addEventListener('change', async e => {
    try {
      await api('/admin/api/config/automode', { method: 'POST', body: { model: e.target.value } });
      toast('automode saved');
    } catch (err) {
      toast('automode save failed: ' + err.message);
    }
  });

  const searchProvider = document.getElementById('search-provider');
  const searchInput = document.getElementById('search-url');
  const saveSearch = async url => {
    try {
      const out = await api('/admin/api/config/search', { method: 'POST', body: { url } });
      searchInput.value = out.url || '';
      toast('search saved');
    } catch (err) {
      toast('search save failed: ' + err.message);
    }
  };
  searchProvider.addEventListener('change', () => {
    const custom = searchProvider.value === 'custom';
    searchInput.readOnly = !custom;
    if (!custom) {
      searchInput.value = searchProvider.value;
      saveSearch(searchProvider.value);
    } else {
      searchInput.focus();
    }
  });
  document.getElementById('search-form').addEventListener('submit', e => {
    e.preventDefault();
    saveSearch(searchInput.value.trim());
  });

  const cycleList = document.getElementById('cycle-list');
  const renumber = () => {
    [...cycleList.querySelectorAll('.cycle-option')].forEach((row, i) => {
      const pos = row.querySelector('.pos');
      if (pos) pos.textContent = i + 1;
    });
  };
  const saveCycle = async () => {
    const ids = [...cycleList.querySelectorAll('input:checked')].map(i => i.value);
    try {
      await api('/admin/api/config/cycle', { method: 'POST', body: { models: ids } });
      toast('cycle saved');
    } catch (err) {
      toast('cycle save failed: ' + err.message);
    }
  };
  cycleList.addEventListener('click', e => {
    const button = e.target.closest('button[data-move]');
    if (!button) return;
    const row = button.closest('.cycle-option');
    if (button.dataset.move === 'up' && row.previousElementSibling) row.previousElementSibling.before(row);
    if (button.dataset.move === 'down' && row.nextElementSibling) row.nextElementSibling.after(row);
    renumber();
    saveCycle();
  });
  cycleList.addEventListener('change', saveCycle);
  document.getElementById('cycle-form').addEventListener('submit', e => {
    e.preventDefault();
    saveCycle();
  });
}

// --- chrome workers ------------------------------------------------------

async function renderChromes(gen) {
  view.innerHTML = `<section class="sheet"><div class="sheet-head"><h2>Workers</h2><span class="sub" id="chrome-state">checking</span></div><div id="chromes"><div class="row static"><span class="id">Loading Chrome captcha status…</span></div></div></section>`;
  const box = document.getElementById('chromes');
  const stateEl = document.getElementById('chrome-state');
  const fmtLast = t => {
    const d = new Date(t);
    return (!t || isNaN(d) || d.getFullYear() < 2000) ? 'never' : d.toLocaleTimeString();
  };
  const tile = (big, lbl) => `<div class="tile"><div class="lbl">${lbl}</div><div class="big">${big}</div></div>`;

  async function post(body) {
    await api('/admin/api/chromes', { method: 'POST', body });
  }

  async function draw() {
    if (gen !== renderGen) return;
    try {
      const status = await api('/admin/api/chromes');
      const list = status.chromes || [];
      const transitioning = status.state === 'starting' || status.state === 'stopping';
      const running = list.filter(c => !c.killed).length;
      const busy = list.filter(c => c.busy && !c.killed).length;
      const extracts = list.reduce((n, c) => n + (c.extracts || 0), 0);
      if (stateEl) {
        const word = transitioning ? status.state : status.enabled ? 'enabled' : (status.state === 'stopped' ? 'stopped' : 'idle');
        const cls = transitioning ? 'warn' : status.enabled ? 'ok' : 'mute';
        stateEl.innerHTML = badge(cls, word);
      }
      box.innerHTML =
        '<div class="tiles">' +
        tile(status.ready || 0, 'ready tokens') +
        tile(running + ' / ' + list.length, 'workers running') +
        tile(busy, 'in use now') +
        tile(extracts, 'captcha extracts') +
        '</div>' +
        (status.error ? '<div class="note" style="padding:.5rem .9rem 0">last error: ' + esc(status.error) + '</div>' : '') +
        (list.length
          ? list.map(c =>
              '<div class="row static" data-id="chrome' + c.index + '">' +
              '<span class="chip">#' + c.index + '</span>' +
              (c.killed ? '<span class="badge sq">killed</span>' : '<span class="badge ok">running</span>') +
              (c.busy && !c.killed ? '<span class="badge warn">in use</span>' : '') +
              (c.warmed && !c.killed ? '<span class="badge mute">warm</span>' : '') +
              '<span class="name" style="justify-content:flex-end;color:var(--dim);font-size:.75rem">' +
              (c.killed ? 'pid freed' : 'pid ' + c.pid) + ' · ' + (c.extracts || 0) +
              ' extracts · last ok ' + fmtLast(c.last_ok) + '</span>' +
              '<button type="button" class="btn-sm" data-index="' + c.index + '" data-mode="' + (c.killed ? 'start' : 'kill') + '">' +
              (c.killed ? 'start' : 'kill') + '</button></div>').join('')
          : '<div class="row static"><span class="id">No Chrome processes allocated. Start All launches the pool, and kill any worker to hand its RAM back.</span></div>') +
        '<div class="sheet-foot">' +
        '<button type="button" class="primary" data-mode-all="start"' + (transitioning ? ' disabled' : '') + '>' +
        (status.enabled ? 'Start All' : 'Turn On / Start All') + '</button>' +
        '<button type="button" data-mode-all="kill"' + (status.state === 'stopping' || (!status.enabled && status.state !== 'starting') ? ' disabled' : '') + '>Kill All</button></div>';
    } catch (err) {
      box.textContent = 'chromes error: ' + err.message;
    }
    if (gen === renderGen) setTimeout(draw, 5000);
  }

  box.addEventListener('click', async e => {
    const all = e.target.closest('button[data-mode-all]');
    if (all) {
      all.disabled = true;
      try {
        await post({ mode: all.dataset.modeAll, all: true });
        toast(all.dataset.modeAll === 'kill' ? 'killed all chromes (RAM freed)' : 'started all chromes');
        draw();
      } catch (err) {
        toast('error: ' + err.message);
        all.disabled = false;
      }
      return;
    }
    const btn = e.target.closest('button[data-mode]');
    if (!btn) return;
    btn.disabled = true;
    try {
      await post({ index: +btn.dataset.index, mode: btn.dataset.mode });
      toast(btn.dataset.mode === 'kill' ? 'killed chrome ' + btn.dataset.index + ' (RAM freed)' : 'started chrome ' + btn.dataset.index);
      draw();
    } catch (err) {
      toast('error: ' + err.message);
      btn.disabled = false;
    }
  });

  draw();
}

route();
