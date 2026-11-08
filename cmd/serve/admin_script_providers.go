package main

const adminProviderScript = `
// --- Provider v2 Editor and List Management ---
let providersV2Data = (typeof PROVIDERS_V2 !== 'undefined' && Array.isArray(PROVIDERS_V2)) ? PROVIDERS_V2 : [];

function escHTML(str) {
  if (str == null) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function secToNs(sec) {
  const n = parseFloat(sec);
  return (isNaN(n) || n <= 0) ? 0 : Math.round(n * 1e9);
}

function nsToSec(ns) {
  return (ns && ns > 0) ? (ns / 1e9) : '';
}

const pEditor = document.getElementById('provider-editor');
const pListEl = document.getElementById('provider-list');
const pEditId = document.getElementById('p-edit-id');
const pIdInput = document.getElementById('p-id');
const pNameInput = document.getElementById('p-name');
const pBaseUrlInput = document.getElementById('p-base-url');
const pProtocolInput = document.getElementById('p-protocol');
const pAuthSchemeInput = document.getElementById('p-auth-scheme');
const pAuthKeyInput = document.getElementById('p-auth-key');
const pAuthKeyHint = document.getElementById('p-auth-key-hint');
const pAuthKeyEnvInput = document.getElementById('p-auth-key-env');
const pMaxConcurrencyInput = document.getElementById('p-max-concurrency');
const pTimeoutConnectInput = document.getElementById('p-timeout-connect');
const pTimeoutQueueInput = document.getElementById('p-timeout-queue');
const pTimeoutIdleInput = document.getElementById('p-timeout-idle');
const pTlsInput = document.getElementById('p-tls');
const pCancelBtn = document.getElementById('cancel-editor-btn');
const pTestEditorBtn = document.getElementById('test-editor-btn');
const editorTestBadge = document.getElementById('editor-test-badge');
const mmList = document.getElementById('model-mappings-list');
const hdrList = document.getElementById('custom-headers-list');
const addModelBtn = document.getElementById('add-model-btn');
const addHdrBtn = document.getElementById('add-header-btn');
const editorHeading = document.getElementById('editor-heading');

function addModelMappingRow(alias = '', upstream = '', hidden = false, fallbacks = '') {
  if (!mmList) return;
  const row = document.createElement('div');
  row.className = 'mm-row';
  row.innerHTML = '<input class="mm-alias" placeholder="Alias (e.g. gpt-4o)" value="' + escHTML(alias) + '" required>' +
    '<input class="mm-upstream" placeholder="Upstream Model (e.g. openai/gpt-4o)" value="' + escHTML(upstream) + '">' +
    '<label title="Hide from client catalog"><input type="checkbox" class="mm-hidden"' + (hidden ? ' checked' : '') + '> Hide</label>' +
    '<input class="mm-fallbacks" placeholder="Fallbacks (comma-separated)" value="' + escHTML(fallbacks) + '">' +
    '<button type="button" class="del-row-btn" title="Remove model mapping">×</button>';
  row.querySelector('.del-row-btn').onclick = () => row.remove();
  mmList.appendChild(row);
}

if (addModelBtn) {
  addModelBtn.onclick = () => addModelMappingRow();
}

function addHeaderRow(name = '', value = '') {
  if (!hdrList) return;
  const row = document.createElement('div');
  row.className = 'hdr-row';
  row.innerHTML = '<input class="hdr-name" placeholder="Header Name (e.g. HTTP-Referer)" value="' + escHTML(name) + '">' +
    '<input class="hdr-val" placeholder="Header Value" value="' + escHTML(value) + '">' +
    '<button type="button" class="del-row-btn" title="Remove header">×</button>';
  row.querySelector('.del-row-btn').onclick = () => row.remove();
  hdrList.appendChild(row);
}

if (addHdrBtn) {
  addHdrBtn.onclick = () => addHeaderRow();
}

if (pAuthSchemeInput) {
  pAuthSchemeInput.addEventListener('change', () => {
    const scheme = pAuthSchemeInput.value;
    if (scheme === 'none') {
      if (pAuthKeyInput) { pAuthKeyInput.disabled = true; pAuthKeyInput.value = ''; }
      if (pAuthKeyEnvInput) { pAuthKeyEnvInput.disabled = true; pAuthKeyEnvInput.value = ''; }
      if (pAuthKeyHint) pAuthKeyHint.textContent = '(disabled for none)';
    } else {
      if (pAuthKeyInput) pAuthKeyInput.disabled = false;
      if (pAuthKeyEnvInput) pAuthKeyEnvInput.disabled = false;
      const isEdit = pEditId && pEditId.value;
      if (pAuthKeyHint) pAuthKeyHint.textContent = isEdit ? '(leave blank to keep existing key)' : '(password/write-only)';
    }
  });
}

function renderTestBadge(el, data) {
  if (!el) return;
  if (data && data.ok) {
    const lat = data.latency_ms != null ? '<span class="dim">' + data.latency_ms + 'ms</span> ' : '';
    const msg = data.message || 'Connection successful';
    el.innerHTML = '<span class="badge ok">OK</span> ' + lat + '<span style="color:var(--on);font-size:0.8rem">' + escHTML(msg) + '</span>';
  } else {
    const lat = (data && data.latency_ms != null) ? '<span class="dim">' + data.latency_ms + 'ms</span> ' : '';
    const err = (data && (data.error || data.status)) || 'Connection failed';
    el.innerHTML = '<span class="badge">FAIL</span> ' + lat + '<span style="color:var(--bad);font-size:0.8rem">' + escHTML(err) + '</span>';
  }
}

function loadProviderForEdit(id) {
  const p = providersV2Data.find(x => x.id === id);
  if (!p || !pEditor) return;
  if (pEditId) pEditId.value = p.id;
  if (editorHeading) editorHeading.textContent = 'Edit Upstream Provider (' + p.id + ')';
  if (pIdInput) {
    pIdInput.value = p.id;
    pIdInput.readOnly = true;
  }
  if (pNameInput) pNameInput.value = p.name || '';
  if (pBaseUrlInput) pBaseUrlInput.value = p.base_url || '';
  if (pProtocolInput) pProtocolInput.value = p.protocol || 'openai-chat';
  
  let scheme = (p.auth && p.auth.scheme) || 'bearer';
  if (scheme === 'api-key') scheme = 'x-api-key';
  if (pAuthSchemeInput) pAuthSchemeInput.value = scheme;

  if (pAuthKeyInput) {
    pAuthKeyInput.value = '';
    if (scheme === 'none') {
      pAuthKeyInput.disabled = true;
      if (pAuthKeyEnvInput) pAuthKeyEnvInput.disabled = true;
      if (pAuthKeyHint) pAuthKeyHint.textContent = '(disabled for none)';
    } else {
      pAuthKeyInput.disabled = false;
      if (pAuthKeyEnvInput) pAuthKeyEnvInput.disabled = false;
      const hasKey = p.auth && (p.auth.key || p.auth.key_ref);
      if (pAuthKeyHint) pAuthKeyHint.textContent = hasKey ? '(current: ' + (p.auth.key || '[REDACTED]') + ' - leave blank to keep)' : '(password/write-only)';
    }
  }
  if (pAuthKeyEnvInput) pAuthKeyEnvInput.value = (p.auth && p.auth.key_env) || '';

  if (mmList) {
    mmList.innerHTML = '';
    if (p.models && p.models.length > 0) {
      p.models.forEach(m => addModelMappingRow(m.alias, m.upstream_model, m.hidden, (m.fallbacks || []).join(', ')));
    } else {
      addModelMappingRow();
    }
  }

  if (hdrList) {
    hdrList.innerHTML = '';
    if (p.headers) {
      Object.keys(p.headers).forEach(k => addHeaderRow(k, p.headers[k]));
    }
  }

  if (pMaxConcurrencyInput) pMaxConcurrencyInput.value = p.max_concurrency || '';
  if (pTimeoutConnectInput) pTimeoutConnectInput.value = nsToSec(p.timeouts && p.timeouts.connect);
  if (pTimeoutQueueInput) pTimeoutQueueInput.value = nsToSec(p.queue_timeout);
  if (pTimeoutIdleInput) pTimeoutIdleInput.value = nsToSec(p.timeouts && p.timeouts.idle);
  if (pTlsInput) pTlsInput.value = p.tls || 'default';

  if (pCancelBtn) pCancelBtn.style.display = 'inline-block';
  if (editorTestBadge) editorTestBadge.innerHTML = '';
  pEditor.scrollIntoView({ behavior: 'smooth' });
}

function resetEditorForm() {
  if (!pEditor) return;
  if (pEditId) pEditId.value = '';
  if (editorHeading) editorHeading.textContent = 'Add Upstream Provider';
  if (pIdInput) {
    pIdInput.value = '';
    pIdInput.readOnly = false;
  }
  if (pNameInput) pNameInput.value = '';
  if (pBaseUrlInput) pBaseUrlInput.value = '';
  if (pProtocolInput) pProtocolInput.value = 'openai-chat';
  if (pAuthSchemeInput) pAuthSchemeInput.value = 'bearer';
  if (pAuthKeyInput) {
    pAuthKeyInput.value = '';
    pAuthKeyInput.disabled = false;
  }
  if (pAuthKeyHint) pAuthKeyHint.textContent = '(password/write-only)';
  if (pAuthKeyEnvInput) {
    pAuthKeyEnvInput.value = '';
    pAuthKeyEnvInput.disabled = false;
  }
  if (mmList) {
    mmList.innerHTML = '';
    addModelMappingRow();
  }
  if (hdrList) hdrList.innerHTML = '';
  if (pMaxConcurrencyInput) pMaxConcurrencyInput.value = '';
  if (pTimeoutConnectInput) pTimeoutConnectInput.value = '';
  if (pTimeoutQueueInput) pTimeoutQueueInput.value = '';
  if (pTimeoutIdleInput) pTimeoutIdleInput.value = '';
  if (pTlsInput) pTlsInput.value = 'default';
  if (pCancelBtn) pCancelBtn.style.display = 'none';
  if (editorTestBadge) editorTestBadge.innerHTML = '';
}

if (pCancelBtn) {
  pCancelBtn.onclick = resetEditorForm;
}

function collectFormData() {
  const editId = pEditId ? pEditId.value.trim() : '';
  const isEdit = !!editId;
  const id = isEdit ? editId : (pIdInput ? pIdInput.value.trim() : '');
  const name = pNameInput ? pNameInput.value.trim() : '';
  const baseUrl = pBaseUrlInput ? pBaseUrlInput.value.trim() : '';
  const protocol = pProtocolInput ? pProtocolInput.value : 'openai-chat';
  let authScheme = pAuthSchemeInput ? pAuthSchemeInput.value : 'bearer';
  if (authScheme === 'api-key') authScheme = 'x-api-key';

  const key = pAuthKeyInput ? pAuthKeyInput.value.trim() : '';
  const keyEnv = pAuthKeyEnvInput ? pAuthKeyEnvInput.value.trim() : '';

  const auth = { scheme: authScheme };
  if (authScheme !== 'none') {
    if (key) auth.key = key;
    if (keyEnv) auth.key_env = keyEnv;
  }

  const existing = isEdit ? providersV2Data.find(x => x.id === id) : null;
  if (isEdit && existing && existing.auth && existing.auth.header_name) {
    auth.header_name = existing.auth.header_name;
  }

  const models = [];
  if (mmList) {
    mmList.querySelectorAll('.mm-row').forEach(r => {
      const aliasInput = r.querySelector('.mm-alias');
      const upstreamInput = r.querySelector('.mm-upstream');
      const hiddenInput = r.querySelector('.mm-hidden');
      const fbInput = r.querySelector('.mm-fallbacks');
      const alias = aliasInput ? aliasInput.value.trim() : '';
      const upstream = upstreamInput ? upstreamInput.value.trim() : '';
      const hidden = hiddenInput ? hiddenInput.checked : false;
      const fbStr = fbInput ? fbInput.value.trim() : '';
      const fallbacks = fbStr ? fbStr.split(',').map(s => s.trim()).filter(Boolean) : [];
      if (alias) {
        const upstreamModel = alias.endsWith('/*') ? upstream : (upstream || alias);
        const existingModel = (isEdit && existing && Array.isArray(existing.models))
          ? existing.models.find(m => m.alias === alias)
          : null;
        const item = existingModel ? JSON.parse(JSON.stringify(existingModel)) : {};
        item.alias = alias;
        item.upstream_model = upstreamModel;
        if (hidden) {
          item.hidden = true;
        } else {
          delete item.hidden;
        }
        if (fallbacks.length > 0) {
          item.fallbacks = fallbacks;
        } else {
          delete item.fallbacks;
        }
        models.push(item);
      }
    });
  }

  const headers = {};
  if (hdrList) {
    hdrList.querySelectorAll('.hdr-row').forEach(r => {
      const hnInput = r.querySelector('.hdr-name');
      const hvInput = r.querySelector('.hdr-val');
      const hn = hnInput ? hnInput.value.trim() : '';
      const hv = hvInput ? hvInput.value : '';
      if (hn) headers[hn] = hv;
    });
  }

  const maxConcurrency = pMaxConcurrencyInput ? (parseInt(pMaxConcurrencyInput.value, 10) || 0) : 0;
  const connSec = pTimeoutConnectInput ? secToNs(pTimeoutConnectInput.value) : 0;
  const idleSec = pTimeoutIdleInput ? secToNs(pTimeoutIdleInput.value) : 0;
  const queueSec = pTimeoutQueueInput ? secToNs(pTimeoutQueueInput.value) : 0;
  const tls = (pTlsInput && pTlsInput.value) ? pTlsInput.value : 'default';

  let payload;
  if (isEdit && existing) {
    payload = JSON.parse(JSON.stringify(existing));
    payload.id = id;
    payload.name = name;
    payload.base_url = baseUrl;
    payload.protocol = protocol;
    payload.auth = auth;
    payload.models = models;
    payload.headers = Object.keys(headers).length > 0 ? headers : undefined;
    payload.max_concurrency = maxConcurrency;
    payload.queue_timeout = queueSec;
    payload.tls = tls;
    if (!payload.timeouts) payload.timeouts = {};
    if (connSec > 0) payload.timeouts.connect = connSec;
    else delete payload.timeouts.connect;
    if (idleSec > 0) payload.timeouts.idle = idleSec;
    else delete payload.timeouts.idle;
    if (Object.keys(payload.timeouts).length === 0) delete payload.timeouts;
  } else {
    const timeouts = {};
    if (connSec > 0) timeouts.connect = connSec;
    if (idleSec > 0) timeouts.idle = idleSec;
    payload = {
      id: id,
      name: name,
      base_url: baseUrl,
      protocol: protocol,
      auth: auth,
      models: models,
      headers: Object.keys(headers).length > 0 ? headers : undefined,
      max_concurrency: maxConcurrency,
      timeouts: (timeouts.connect || timeouts.idle) ? timeouts : undefined,
      queue_timeout: queueSec,
      tls: tls,
      enabled: true,
    };
  }
  return { isEdit, id, payload };
}

if (pEditor) {
  pEditor.onsubmit = async (e) => {
    e.preventDefault();
    const { isEdit, id, payload } = collectFormData();
    if (!id || !payload.name || !payload.base_url) {
      flash('Please fill in Provider ID, Name, and Base URL');
      return;
    }
    try {
      const url = isEdit ? '/admin/api/providers/' + encodeURIComponent(id) : '/admin/api/providers';
      const method = isEdit ? 'PUT' : 'POST';
      const res = await fetch(url, {
        method: method,
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) {
        flash((isEdit ? 'Updated' : 'Created') + ' provider ' + id);
        setTimeout(() => location.reload(), 400);
      } else {
        const err = await res.json().catch(() => ({}));
        flash('Save failed: ' + (err.error || res.statusText || res.status));
      }
    } catch (err) {
      flash('Save failed: ' + err);
    }
  };
}

if (pTestEditorBtn) {
  pTestEditorBtn.onclick = async () => {
    const { isEdit, id, payload } = collectFormData();
    if (!payload.base_url) {
      flash('Base URL is required to test');
      return;
    }
    if (editorTestBadge) {
      editorTestBadge.innerHTML = '<span class="dim">Testing connection...</span>';
    }
    try {
      let url = '/admin/api/providers/test';
      if (isEdit && id) {
        url = '/admin/api/providers/' + encodeURIComponent(id) + '/test';
      }
      const res = await fetch(url, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(payload),
      });
      const data = await res.json().catch(() => ({}));
      renderTestBadge(editorTestBadge, data);
    } catch (err) {
      if (editorTestBadge) {
        editorTestBadge.innerHTML = '<span class="badge">FAIL</span> <span style="color:var(--bad)">' + escHTML(String(err)) + '</span>';
      }
    }
  };
}

async function testProviderConnection(id) {
  const badgeEl = document.getElementById('test-res-' + id);
  if (badgeEl) badgeEl.innerHTML = '<span class="dim">Testing...</span>';
  try {
    const res = await fetch('/admin/api/providers/' + encodeURIComponent(id) + '/test', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
    });
    const data = await res.json().catch(() => ({}));
    renderTestBadge(badgeEl, data);
  } catch (err) {
    if (badgeEl) badgeEl.innerHTML = '<span class="badge">FAIL</span> <span style="color:var(--bad)">' + escHTML(String(err)) + '</span>';
  }
}

async function toggleProviderEnabled(id, newStatus) {
  const p = providersV2Data.find(x => x.id === id);
  if (!p) return;
  const updated = Object.assign({}, p, { enabled: newStatus });
  try {
    const res = await fetch('/admin/api/providers/' + encodeURIComponent(id), {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(updated),
    });
    if (res.ok) {
      p.enabled = newStatus;
      flash((newStatus ? 'Enabled ' : 'Disabled ') + id);
    } else {
      const err = await res.json().catch(() => ({}));
      flash('Toggle failed: ' + (err.error || res.status));
      setTimeout(() => location.reload(), 400);
    }
  } catch (err) {
    flash('Toggle failed: ' + err);
  }
}

async function deleteProviderById(id) {
  if (!confirm('Delete provider "' + id + '"?')) return;
  try {
    const res = await fetch('/admin/api/providers/' + encodeURIComponent(id), {
      method: 'DELETE',
    });
    if (res.ok) {
      flash('Deleted provider ' + id);
      setTimeout(() => location.reload(), 400);
    } else {
      const err = await res.json().catch(() => ({}));
      flash('Delete failed: ' + (err.error || res.status));
    }
  } catch (err) {
    flash('Delete failed: ' + err);
  }
}

if (pListEl) {
  pListEl.addEventListener('click', (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    const id = btn.dataset.id;
    if (!id) return;
    if (btn.classList.contains('edit-prov-btn')) {
      loadProviderForEdit(id);
    } else if (btn.classList.contains('test-prov-btn')) {
      testProviderConnection(id);
    } else if (btn.classList.contains('delete-prov-btn')) {
      deleteProviderById(id);
    }
  });

  pListEl.addEventListener('change', (e) => {
    if (e.target.classList.contains('prov-toggle')) {
      const id = e.target.dataset.id;
      if (id) {
        toggleProviderEnabled(id, e.target.checked);
      }
    }
  });
}

if (mmList && mmList.children.length === 0) {
  addModelMappingRow();
}

async function syncProvidersFromAPI() {
  try {
    const res = await fetch('/admin/api/providers');
    if (res.ok) {
      const list = await res.json();
      if (Array.isArray(list)) {
        providersV2Data = list;
      }
    }
  } catch (_) {}
}
if (PAGE === 'models' || PAGE === '' || !PAGE) {
  syncProvidersFromAPI();
}
`
