package main

const adminStatsScript = `
// --- Model stats: KPI tiles + line charts (crosshair, tooltip). Refresh 5s.
// Palette validated: #3987e5/#d95926 on the #171a21 surface; status badges
// use --on/--bad.
if (PAGE === 'dashboard') {
  const charts = document.getElementById('charts');
  const fmtTime = t => { const d = new Date(t); return d.getFullYear() > 1 ? d.toLocaleTimeString() : '—'; };
  const fmtMS = ms => ms >= 1000 ? (ms / 1000).toFixed(1) + 's' : (ms || 0) + 'ms';
  const fmtN = n => (n || 0).toLocaleString();
  const fmtUp = ms => { if (!(ms > 0)) return '—'; const s = Math.floor(ms / 1000), h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60);
    return (h ? h + 'g ' : '') + (h || m ? m + 'p ' : '') + (h ? '' : (s % 60) + 's'); };
  const esc = s => String(s).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
  // Provider badge colors: only from the validated palette / existing CSS tokens.
  const PCOLS = ['#3987e5', '#d95926', 'var(--on)', 'var(--warn)', 'var(--dim)'];
  function provCell(m) {
    m = m || '';
    const i = m.indexOf('/');
    if (i < 1) return esc(m);
    const p = m.slice(0, i);
    let h = 0; for (let j = 0; j < p.length; j++) h = (h * 31 + p.charCodeAt(j)) >>> 0;
    const c = PCOLS[h % PCOLS.length];
    return '<span class=pb style="color:' + c + ';border-color:' + c + '">' + esc(p) + '</span>' + esc(m.slice(i + 1));
  }
  const C = {req:'#3987e5', in:'#3987e5', out:'#d95926'}; // slot1 blue / slot2 orange
  const MCOLS = ['#3987e5', '#d95926', '#2da44e', '#bf4b52', '#8250df', '#1b7c83', '#e3b341', '#218bff']; // per-model chart lines
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

  // Tile sparkline: last 12 points, series-coloured line, no fill.
  function spark(vals, color) {
    const v = vals.slice(-12), max = Math.max(1, ...v);
    const W = 96, H = 26;
    const x = i => 1 + i * (W - 2) / Math.max(1, v.length - 1);
    const y = t => H - 2 - t * (H - 4) / max;
    const pts = v.map((t, i) => x(i).toFixed(1) + ',' + y(t).toFixed(1)).join(' ');
    return '<svg width=' + W + ' height=' + H + ' viewBox="0 0 ' + W + ' ' + H + '" style="display:block">' +
      '<polyline points="' + pts + '" fill="none" stroke="' + color + '" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>' +
      '<circle cx="' + x(v.length - 1).toFixed(1) + '" cy="' + y(v[v.length - 1]).toFixed(1) + '" r="4" fill="' + color + '" stroke="#171a21" stroke-width="2"/>' +
      '</svg>';
  }

  // Line chart with crosshair + tooltip. defs: [{key,label,color,fmt}].
  // One Y axis per chart (no dual axis); tooltip shows that chart's series.
  let chartDefs = [], chartData = [];
  function lineChart(defs, data) {
    const W = 640, H = 150, PADL = 38, PADR = 10, PADT = 8, PADB = 20;
    if (!data) data = series;
    const all = defs.map(d => data.map(p => p[d.key] || 0));
    const max = Math.max(1, ...all.flat());
    const tick = max > 4 ? Math.ceil(max / 4) : 1;
    const yMax = Math.max(tick, Math.ceil(max / tick) * tick);
    const x = i => PADL + i * (W - PADL - PADR) / Math.max(1, data.length - 1);
    const y = v => H - PADB - v * (H - PADB - PADT) / yMax;
    let grid = '';
    for (let t = 0; t <= 4; t++) {
      const v = yMax * t / 4, yy = y(v).toFixed(1);
      grid += '<line x1=' + PADL + ' x2=' + (W - PADR) + ' y1=' + yy + ' y2=' + yy + ' stroke="#242833" stroke-width="1"/>' +
        '<text x=' + (PADL - 6) + ' y=' + (yy * 1 + 4) + ' text-anchor="end" class=ax>' + fmtN(Math.round(v)) + '</text>';
    }
    let paths = '', legend = '';
    defs.forEach((d, di) => {
      const vals = data.map(p => p[d.key] || 0);
      const pts = vals.map((v, i) => x(i).toFixed(1) + ',' + y(v).toFixed(1)).join(' ');
      paths += '<path d="M' + x(0).toFixed(1) + ',' + y(vals[0] || 0).toFixed(1) +
        ' L' + pts.replace(/ /g, ' L') + '" fill="none" stroke="' + d.color + '" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>';
      // end-dot with a 2px surface ring
      paths += '<circle cx=' + x(vals.length - 1).toFixed(1) + ' cy=' + y(vals[vals.length - 1] || 0).toFixed(1) + ' r=4 fill=' + d.color + ' stroke="#171a21" stroke-width="2"/>';
      legend += '<span class=lg><svg width=14 height=8><line x1=0 y1=4 x2=14 y2=4 stroke=' + d.color + ' stroke-width=2 stroke-linecap=round/></svg>' + d.label + '</span>';
    });
    chartDefs.push(defs); chartData.push(data);
    return '<div class=chartbox><div class=chhead>' + legend + '</div>' +
      '<div class=chwrap data-chart>' +
      '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" style="width:100%;height:150px;display:block">' +
      grid + paths +
      '<line class=crosshair y1=' + PADT + ' y2=' + (H - PADB) + ' x1=-99 x2=-99 stroke="#8b93a7" stroke-width="1"/>' +
      '</svg><div class=tip hidden></div></div></div>';
  }

  function sortHeader(cols, key, dir, attr) {
    return '<tr>' + cols.map(c => '<th ' + attr + '="' + c[0] + '"' +
      (c[2] ? ' title="' + esc(c[2]) + '"' : '') +
      (c[0] === key ? ' class="on' + (dir > 0 ? ' asc' : '') + '"' : '') + '>' + c[1] + '</th>').join('') + '</tr>';
  }

  function drawCharts() {
    chartDefs = []; chartData = [];
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
      [fmtN(tot('output_tokens')), 'tokens ra', series.map(p => p.output_tokens || 0), C.out],
      [fmtN(tot('errors')) + ' (' + errPct + '%)', 'error', series.map(p => p.errors || 0), '#e66767'],
      [ttfbAvg ? fmtMS(ttfbAvg) : '—', 'TTFB tb (stream)', null, null],
      [mss.length ? fmtMS(p95) : '—', 'p95 latency', null, null],
      [fmtUp(startedAt ? Date.now() - startedAt : 0), 'uptime', null, null],
    ];
    charts.innerHTML =
      '<div class=tiles>' + tiles.map(t =>
        '<div class=tile><div class=lbl>' + t[1] + '</div><div class=big>' + t[0] + '</div>' +
        (t[2] ? spark(t[2], t[3]) : '') + '</div>').join('') + '</div>' +
      '<div class=note>all metrics since server start' +
        (startedAt ? ' (' + new Date(startedAt).toLocaleString() + ')' : '') +
        ' — every API the gateway serves: both NVIDIA playground models and custom providers via passthrough (tokenharbor, justworker…) all count.</div>' +
      '<div class=chartrow>' +
      lineChart([{key:'requests', label:'requests / min', color:C.req}]) +
      lineChart([{key:'input_tokens', label:'in tok / min', color:C.in}, {key:'output_tokens', label:'out tok / min', color:C.out}]) +
      lineChart([{key:'ttfb_avg_ms', label:'avg TTFB / min', color:C.req, fmt:fmtMS}]) +
      '</div>' +
      '<div class=note>TTFB = time to first byte of the stream; minutes with no streamed requests show 0.</div>' +
      '<div class=note><b>per-model charts — requests / min</b></div>' +
      (Object.keys(modelSeries).length
        ? (() => {
            const top = Object.entries(modelSeries)
              .map(([m, s]) => [m, s.reduce((a, p) => a + (p.requests || 0), 0)])
              .sort((a, b) => b[1] - a[1]).slice(0, 8).map(x => x[0]);
            // fixed colour by model-name hash — matches the provCell table, stable per render
            const mcol = m => { let h = 0; for (let j = 0; j < m.length; j++) h = (h * 31 + m.charCodeAt(j)) >>> 0; return MCOLS[h % MCOLS.length]; };
            const grid = series.map((p, i) => {
              const o = {minute: p.minute};
              top.forEach((m, k) => { o['m' + k] = (modelSeries[m] && modelSeries[m][i] && modelSeries[m][i].requests) || 0; });
              return o;
            });
            return lineChart(top.map((m, k) => ({key: 'm' + k, label: esc(m), color: mcol(m)})), grid);
          })()
        : '<div class=row><span class=id>no requests yet</span></div>');
    armCharts();
  }

  // Crosshair + tooltip: snap to nearest X, show that chart's series at X.
  // Tooltip uses textContent (model id values are untrusted).
  function armCharts() {
    charts.querySelectorAll('.chwrap').forEach((w, ci) => {
      const svg = w.querySelector('svg'), cross = svg.querySelector('.crosshair'), tip = w.querySelector('.tip');
      const defs = chartDefs[ci] || [];
      const move = e => {
        const r = svg.getBoundingClientRect();
        const px = (e.clientX - r.left) / r.width * 640;
        const i = Math.max(0, Math.min(series.length - 1, Math.round((px - 38) / ((640 - 38 - 10) / Math.max(1, series.length - 1)))));
        const vx = 38 + i * (640 - 38 - 10) / Math.max(1, series.length - 1);
        cross.setAttribute('x1', vx); cross.setAttribute('x2', vx);
        const p = (chartData[ci] || series)[i]; if (!p) return;
        tip.textContent = '';
        const head = document.createElement('div');
        head.className = 'tip-h';
        head.textContent = new Date(p.minute).toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'});
        tip.appendChild(head);
        defs.forEach(d => {
          const row = document.createElement('div'); row.className = 'tip-r';
          const key = document.createElement('span'); key.className = 'tip-k';
          const sw = document.createElement('span'); sw.className = 'tip-sw'; sw.style.background = d.color;
          key.appendChild(sw); key.appendChild(document.createTextNode(d.label));
          const val = document.createElement('b'); val.textContent = (d.fmt || fmtN)(p[d.key] || 0);
          row.appendChild(key); row.appendChild(val); tip.appendChild(row);
        });
        tip.classList.remove('hidden');
        const tx = Math.min(r.width - 130, Math.max(4, e.clientX - r.left + 12));
        tip.style.left = tx + 'px'; tip.style.top = '6px';
      };
      w.addEventListener('pointermove', move);
      w.addEventListener('pointerleave', () => { cross.setAttribute('x1', -99); cross.setAttribute('x2', -99); tip.classList.add('hidden'); });
    });
  }

  // Request table: shared by the live log and history frames.
  function logTable(evs, key, dir) {
    const lval = (e, k) => k === 'time' ? +new Date(e.time)
      : k === 'model' ? (e.model || '')
      : typeof e[k] === 'boolean' ? (e[k] ? 1 : 0) : (e[k] || 0);
    const sorted = evs.slice().sort((a, b) => {
      const va = lval(a, key), vb = lval(b, key);
      return (typeof va === 'string' ? va.localeCompare(vb) : va - vb) * dir;
    });
    return '<div class=logwrap><table class=stats><thead>' + sortHeader(LCOLS, key, dir, 'data-lkey') + '</thead><tbody>' +
      sorted.map(e => '<tr>' +
        '<td>' + fmtTime(e.time) + '</td>' +
        '<td class=nm>' + provCell(e.model) + '</td>' +
        '<td>' + (e.streamed ? 'yes' : '—') + '</td>' +
        '<td>' + fmtN(e.input_tokens) + '</td>' +
        '<td>' + fmtN(e.output_tokens) + '</td>' +
        '<td>' + fmtMS(e.ms) + '</td>' +
        '<td>' + (e.ttfb_ms ? fmtMS(e.ttfb_ms) : '—') + '</td>' +
        '<td>' + (e.error ? '<span class=badge>error</span>' : '<span class="badge ok">ok</span>') + '</td>' +
      '</tr>').join('') + '</tbody></table></div>';
  }
  async function poll() {
    try {
      const res = await fetch('/admin/stats');
      if (res.ok) {
        const d = await res.json();
        models = d.models || [];
        series = d.series || [];
        modelSeries = d.series_by_model || {};
        events = d.events || [];
        startedAt = d.started_at ? +new Date(d.started_at) : 0;
        drawCharts();
        totalFrames = d.total_frames || 0;
        if (!curFrame && totalFrames) openFrame(1); else drawFrames(); // poll only updates chip counts (frame 1 = newest)
      } else { charts.textContent = 'stats error: ' + res.status; }
    } catch (err) { charts.textContent = 'stats error: ' + err; }
  }

  // History log frames: #frames = chips 1..total (frame 1 = newest), >12 the
  // « » buttons slide a 12-chip window; content fetched once per click, no polling.
  const framesBox = document.getElementById('frames');
  const frameBox = document.getElementById('framebox');
  let totalFrames = 0, curFrame = 0, fKey = 'time', fDir = -1, fEvents = [];
  function drawFrames() {
    if (!totalFrames) { framesBox.innerHTML = ''; return; }
    let lo = 1, hi = totalFrames, nav = '';
    if (totalFrames > 12) {
      lo = Math.floor((curFrame - 1) / 12) * 12 + 1;
      hi = Math.min(totalFrames, lo + 11);
      nav = '<button type=button data-f="prev">«</button>';
    }
    let chips = '';
    for (let f = lo; f <= hi; f++)
      chips += '<button type=button class="fp' + (f === curFrame ? ' on' : '') + '" data-f=' + f + '>' + f + '</button>';
    framesBox.innerHTML = nav + chips + (totalFrames > 12 ? '<button type=button data-f="next">»</button>' : '');
  }
  async function openFrame(n) {
    n = Math.max(1, Math.min(totalFrames, n));
    curFrame = n; drawFrames();
    try {
      const res = await fetch('/admin/stats?frames=' + n);
      const d = await res.json().catch(() => ({}));
      if (!res.ok) { frameBox.textContent = 'frame error: ' + (d.error || res.status); return; }
      totalFrames = d.total_frames || totalFrames; drawFrames();
      fEvents = d.events || [];
      frameBox.innerHTML = logTable(fEvents, fKey, fDir);
    } catch (err) { frameBox.textContent = 'frame error: ' + err; }
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
    if (fKey === th.dataset.lkey) { fDir = -fDir; } else { fKey = th.dataset.lkey; fDir = -1; }
    frameBox.innerHTML = logTable(fEvents, fKey, fDir);
  });
  poll();
  setInterval(poll, 5000);
} // dashboard block end`
