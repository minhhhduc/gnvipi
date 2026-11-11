package main

const adminChromeScript = `
// --- Captcha chromes (kill = free RAM, start = respawn) ---
if (PAGE === 'chromes') {
  const box = document.getElementById('chromes');
  const stateEl = document.getElementById('chrome-state');
  const fmtLast = t => {
    const d = new Date(t);
    return d.getFullYear() > 1 ? d.toLocaleTimeString() : 'never';
  };
  const tile = (big, lbl) => '<div class=tile><div class=lbl>' + lbl + '</div><div class=big>' + big + '</div></div>';
  async function post(body) {
    const res = await fetch('/admin/chromes', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({error: res.status}));
      throw new Error(err.error);
    }
  }
  async function doMode(btn) {
    btn.disabled = true;
    try {
      await post({index: +btn.dataset.index, mode: btn.dataset.mode});
      flash(btn.dataset.mode === 'kill' ? 'killed chrome ' + btn.dataset.index + ' (RAM freed)'
                                       : 'started chrome ' + btn.dataset.index);
      drawChromes();
    } catch (err) {
      flash('error: ' + err.message);
      btn.disabled = false;
    }
  }
  async function drawChromes() {
    try {
      const res = await fetch('/admin/chromes');
      if (!res.ok) { box.textContent = 'chromes error: ' + res.status; return; }
      const status = await res.json();
      const list = status.chromes || [];
      const transitioning = status.state === 'starting' || status.state === 'stopping';
      const running = list.filter(c => !c.killed).length;
      const busy = list.filter(c => c.busy && !c.killed).length;
      const extracts = list.reduce((n, c) => n + (c.extracts || 0), 0);

      // State reads in the sheet header so the tabs above stay the only nav.
      if (stateEl) {
        const word = transitioning ? status.state
                   : status.enabled ? 'enabled' : (status.state === 'stopped' ? 'stopped' : 'idle');
        const cls = transitioning ? 'warn' : status.enabled ? 'ok' : 'mute';
        stateEl.innerHTML = '<span class="badge ' + cls + '">' + word + '</span>';
      }

      box.innerHTML =
        '<div class=tiles>' +
          tile(status.ready, 'ready tokens') +
          tile(running + ' / ' + list.length, 'workers running') +
          tile(busy, 'in use now') +
          tile(extracts, 'captcha extracts') +
        '</div>' +
        (status.error ? '<div class="note" style="padding:.5rem .9rem 0">last error: ' + status.error + '</div>' : '') +
        (list.length
          ? list.map(c =>
              '<div class="row static" data-id="chrome' + c.index + '">' +
                '<span class=chip>#' + c.index + '</span>' +
                (c.killed ? '<span class="badge sq">killed</span>'
                          : '<span class="badge ok">running</span>') +
                (c.busy && !c.killed ? '<span class="badge warn">in use</span>' : '') +
                (c.warmed && !c.killed ? '<span class="badge mute">warm</span>' : '') +
                '<span class=name style="justify-content:flex-end;color:var(--dim);font-size:.75rem">' +
                  (c.killed ? 'pid freed' : 'pid ' + c.pid) + ' · ' + c.extracts +
                  ' extracts · last ok ' + fmtLast(c.last_ok) + '</span>' +
                '<button type=button class=btn-sm data-index="' + c.index + '" data-mode="' +
                  (c.killed ? 'start' : 'kill') + '">' +
                  (c.killed ? 'start' : 'kill') + '</button>' +
              '</div>').join('')
          : '<div class="row static"><span class=id>No Chrome processes allocated. Start All launches the pool, and kill any worker to hand its RAM back.</span></div>') +
        '<div class=sheet-foot>' +
          '<button type=button class=primary data-mode-all=start' + (transitioning ? ' disabled' : '') + '>' +
            (status.enabled ? 'Start All' : 'Turn On / Start All') + '</button>' +
          '<button type=button data-mode-all=kill' + (status.state === 'stopping' || (!status.enabled && status.state !== 'starting') ? ' disabled' : '') + '>Kill All</button></div>';
    } catch (err) {
      box.textContent = 'chromes error: ' + err;
    }
  }
  box.addEventListener('click', async e => {
    const all = e.target.closest('button[data-mode-all]');
    if (all) {
      all.disabled = true;
      try {
        await post({mode: all.dataset.modeAll, all: true});
        flash(all.dataset.modeAll === 'kill' ? 'killed all chromes (RAM freed)'
                                            : 'started all chromes');
        drawChromes();
      } catch (err) {
        flash('error: ' + err.message);
        all.disabled = false;
      }
      return;
    }
    const btn = e.target.closest('button[data-mode]');
    if (btn) doMode(btn);
  });
  drawChromes();
  setInterval(drawChromes, 5000);
}
</script>`
