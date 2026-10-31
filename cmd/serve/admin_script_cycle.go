package main

const adminCycleScript = `
const cycleForm = document.getElementById('cycle-form');
if (cycleForm) {
  const cycleList = document.getElementById('cycle-list');
  let pending = Promise.resolve();
  const saveCycle = () => {
    const models = [...cycleList.querySelectorAll('input:checked')].map(input => input.value);
    pending = pending.catch(() => {}).then(async () => {
      const res = await fetch('/admin/cycle', {method: 'POST', headers: {'content-type': 'application/json'}, body: JSON.stringify({models})});
      if (!res.ok) {
        const error = await res.json().catch(() => ({}));
        throw new Error(error.error || 'cycle save failed');
      }
      flash('cycle saved');
    }).catch(err => flash(err.message));
  };
  cycleList.addEventListener('click', e => {
    const button = e.target.closest('button[data-move]');
    if (!button) return;
    const row = button.closest('.cycle-option');
    if (button.dataset.move === 'up' && row.previousElementSibling) row.previousElementSibling.before(row);
    if (button.dataset.move === 'down' && row.nextElementSibling) row.nextElementSibling.after(row);
    saveCycle();
  });
  cycleList.addEventListener('change', saveCycle);
  cycleForm.addEventListener('submit', e => {
    e.preventDefault();
    saveCycle();
  });
}
`
