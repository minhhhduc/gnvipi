package main

const adminChromeScript = `
// --- Captcha chromes (kill = free RAM, start = respawn) ---
if (CHROMES && PAGE === 'chromes') {
  const box = document.getElementById('chromes');
  const fmtLast = t => {
    const d = new Date(t);
    return d.getFullYear() > 1 ? d.toLocaleTimeString() : '—';
  };
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
      const list = (await res.json()).chromes || [];
      box.innerHTML = (list.length
        ? list.map(c =>
            '<div class=row data-id="chrome' + c.index + '">' +
              '<span class=id>#' + c.index + (c.killed ? '' : ' · pid ' + c.pid) + '</span>' +
              (c.busy && !c.killed ? '<span class="badge warn">busy</span>' : '') +
              (c.killed ? '<span class="badge" style="color:var(--bad);border-color:var(--bad)">killed</span>'
                        : '<span class="badge ok">running</span>') +
              (c.warmed && !c.killed ? '<span class="badge ok">warm</span>' : '') +
              '<span class=name style="justify-content:flex-end;color:var(--dim);font-size:.78rem">' +
                c.extracts + ' extracts · ok ' + fmtLast(c.last_ok) + '</span>' +
              '<button type=button data-index="' + c.index + '" data-mode="' +
                (c.killed ? 'start' : 'kill') + '">' +
                (c.killed ? 'start' : 'kill') + '</button>' +
            '</div>').join('')
        : '<div class=row><span class=id>no chromes</span>') +
        (list.length ? '<div class=bar style="margin-top:.6rem">' +
          '<button type=button data-mode-all=kill>kill all</button>' +
          '<button type=button data-mode-all=start>start all</button></div>' : '');
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
