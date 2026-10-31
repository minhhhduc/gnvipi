package main

const adminScript = adminBaseScript + adminCycleScript + adminStatsScript + adminChromeScript

// adminScript keeps the page and the mask file in sync: every toggle saves.
const adminBaseScript = `
const autoSelect = document.getElementById('auto-model');
if (autoSelect) autoSelect.addEventListener('change', async () => {
  const res = await fetch('/admin/automode', {method: 'POST', headers: {'content-type': 'application/json'}, body: JSON.stringify({model: autoSelect.value})});
  if (res.ok) location.reload();
  else alert('Could not save automode model');
});
const searchForm = document.getElementById('search-form');
if (searchForm) {
  const provider = document.getElementById('search-provider');
  const searchURL = document.getElementById('search-url');
  provider.value = [...provider.options].some(o => o.value === searchURL.value) ? searchURL.value : 'custom';
  searchURL.readOnly = provider.value !== 'custom';
  provider.addEventListener('change', () => {
    searchURL.readOnly = provider.value !== 'custom';
    if (provider.value !== 'custom') {
      searchURL.value = provider.value;
      saveSearch();
    }
  });
  let pendingSearch = Promise.resolve();
  const saveSearch = () => {
    const url = provider.value === 'custom' ? searchURL.value.trim() : provider.value;
    searchURL.value = url;
    pendingSearch = pendingSearch.catch(() => {}).then(async () => {
      const res = await fetch('/admin/search', {method: 'POST', headers: {'content-type': 'application/json'}, body: JSON.stringify({url})});
      if (!res.ok) {
        const error = await res.json().catch(() => ({}));
        throw new Error(error.error || 'search save failed');
      }
      flash('search saved');
    }).catch(err => flash(err.message));
  };
  searchURL.addEventListener('change', saveSearch);
  searchForm.addEventListener('submit', e => { e.preventDefault(); saveSearch(); });
}
const rows = [...document.querySelectorAll('.row')];
const sws = [...document.querySelectorAll('.sw')];
const counts = document.getElementById('counts');
const toastEl = document.getElementById('toast');
let toastTimer;

function flash(msg) {
  toastEl.textContent = msg;
  toastEl.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toastEl.classList.remove('show'), 1400);
}

function render() {
  const on = sws.filter(s => s.checked).length;
  counts.textContent = on + ' on · ' + (sws.length - on) + ' off · ' + FAILED + ' upstream errors';
}

async function save() {
  render();
  const hidden = sws.filter(s => !s.checked).map(s => s.value);
  try {
    const res = await fetch('/admin/models', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({masks_id: hidden}),
    });
    flash(res.ok ? 'saved' : 'save failed: ' + res.status);
  } catch (err) {
    flash('save failed: ' + err);
  }
}

const addForm = document.getElementById('addform');
if (addForm) addForm.addEventListener('submit', async e => {
  e.preventDefault();
  const f = new FormData(e.target);
  const list = (PROVIDERS || []).concat([{
    name: f.get('name').trim(), base_url: f.get('base_url').trim(),
    api_key: f.get('api_key').trim(), model: f.get('model').trim(),
    responses_only: f.get('responses_only') === 'on',
    messages_only: f.get('messages_only') === 'on',
  }]);
  try {
    const res = await fetch('/admin/providers', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(list),
    });
    if (res.ok) {
      flash('added endpoint');
      setTimeout(() => { location.href = '/admin?page=models'; }, 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('add failed: ' + err.error);
    }
  } catch (err) {
    flash('add failed: ' + err);
  }
});

async function deleteProvider(e, idx) {
  e.preventDefault();
  e.stopPropagation();
  const list = (PROVIDERS || []).filter((_, i) => i !== idx);
  try {
    const res = await fetch('/admin/providers', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(list),
    });
    if (res.ok) {
      flash('deleted endpoint');
      setTimeout(() => { location.href = '/admin?page=models'; }, 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('delete failed: ' + err.error);
    }
  } catch (err) {
    flash('delete failed: ' + err);
  }
}

// --- Delete model ---
async function deleteModel(e, id) {
  e.preventDefault();
  e.stopPropagation();
  try {
    const res = await fetch('/admin/delete', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({id}),
    });
    if (res.ok) {
      flash('deleted: ' + id);
      setTimeout(() => location.reload(), 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('delete failed: ' + err.error);
    }
  } catch (err) {
    flash('delete failed: ' + err);
  }
}

async function restoreModel(id) {
  try {
    const res = await fetch('/admin/delete', {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({id, restore: true}),
    });
    if (res.ok) {
      flash('restored: ' + id);
      setTimeout(() => location.reload(), 300);
    } else {
      const err = await res.json().catch(() => ({error: res.status}));
      flash('restore failed: ' + err.error);
    }
  } catch (err) {
    flash('restore failed: ' + err);
  }
}

// Deleted models section
function drawDeleted() {
  const deleted = DELETED || [];
  if (deleted.length === 0) return;

  const section = document.createElement('div');
  section.id = 'deleted-section';
  section.className = 'collapsed';

  const header = document.createElement('div');
  header.id = 'deleted-header';
  header.innerHTML = '<span class=arrow>▶</span> <span>Deleted models</span> <span class=cnt>' + deleted.length + ' model</span>';
  header.onclick = () => section.classList.toggle('collapsed');

  const list = document.createElement('div');
  list.id = 'deleted-list';
  deleted.forEach(id => {
    const row = document.createElement('div');
    row.className = 'del-row';
    row.innerHTML = '<span class=id>' + id + '</span>';
    const btn = document.createElement('button');
    btn.textContent = 'restore';
    btn.onclick = () => restoreModel(id);
    row.appendChild(btn);
    list.appendChild(row);
  });

  section.appendChild(header);
  section.appendChild(list);
  document.querySelector('main').appendChild(section);
}

drawDeleted();
// Click anywhere on row (except del-btn) toggles the switch.
rows.forEach(r => {
  r.addEventListener('click', e => {
    if (e.target.classList.contains('del-btn')) return;
    if (e.target.classList.contains('sw')) return;
    const sw = r.querySelector('.sw');
    if (sw) { sw.checked = !sw.checked; save(); }
  });
});
sws.forEach(s => s.addEventListener('change', save));
document.getElementById('all').onclick = () => { sws.forEach(s => s.checked = true); save(); };
document.getElementById('none').onclick = () => { sws.forEach(s => s.checked = false); save(); };
document.getElementById('retry').onclick = async () => {
  await fetch('/admin/retry-failed', {method: 'POST'});
  location.reload();
};
document.getElementById('q').addEventListener('input', e => {
  const q = e.target.value.toLowerCase();
  rows.forEach(r => r.classList.toggle('hide', !r.dataset.id.toLowerCase().includes(q)));
});
render();

// Hide search input on non-models pages
if (PAGE !== 'models') {
  document.getElementById('q').style.display = 'none';
  document.getElementById('all').style.display = 'none';
  document.getElementById('none').style.display = 'none';
  document.getElementById('retry').style.display = 'none';
}`
