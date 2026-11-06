'use strict';

const state = {
  token: localStorage.getItem('lgllm.admin.token') || '',
  tab: 'providers',
  logsAbort: null,
  usageGroup: 'provider',
};

const view = document.getElementById('view');
const statusEl = document.getElementById('status');
const tokenEl = document.getElementById('token');

tokenEl.value = state.token;

function setStatus(text, kind) {
  statusEl.textContent = text || '';
  statusEl.className = kind === 'error' ? 'error' : kind === 'ok' ? 'ok' : 'muted';
}

async function api(path, options = {}) {
  const headers = Object.assign({}, options.headers);
  if (state.token) headers['X-Admin-Token'] = state.token;
  if (options.body && !headers['Content-Type']) headers['Content-Type'] = 'application/json';
  const res = await fetch(path, Object.assign({}, options, { headers }));
  if (res.status === 204) return null;
  const text = await res.text();
  let body = null;
  if (text) {
    try { body = JSON.parse(text); } catch (_) { body = text; }
  }
  if (!res.ok) {
    const message = body && body.error && body.error.message ? body.error.message : (typeof body === 'string' ? body : res.statusText);
    const err = new Error(message || ('HTTP ' + res.status));
    err.status = res.status;
    throw err;
  }
  return body;
}

function el(tag, attrs = {}, children = []) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  for (const child of [].concat(children)) {
    if (child === null || child === undefined) continue;
    node.appendChild(typeof child === 'string' ? document.createTextNode(child) : child);
  }
  return node;
}

function badge(text, cls) {
  return el('span', { class: 'pill ' + (cls || ''), text });
}

function openEditor(title, template, onSave) {
  const dlg = el('dialog');
  const area = el('textarea', { spellcheck: 'false' });
  area.value = template;
  const error = el('div', { class: 'error' });
  const save = el('button', { text: 'Save' });
  const cancel = el('button', { text: 'Cancel' });
  cancel.addEventListener('click', () => dlg.close());
  save.addEventListener('click', async () => {
    let parsed;
    try {
      parsed = JSON.parse(area.value);
    } catch (err) {
      error.textContent = 'Invalid JSON: ' + err.message;
      return;
    }
    save.disabled = true;
    try {
      await onSave(parsed);
      dlg.close();
    } catch (err) {
      error.textContent = err.message;
    } finally {
      save.disabled = false;
    }
  });
  dlg.append(el('h3', { text: title }), area, error, el('menu', {}, [cancel, save]));
  document.body.appendChild(dlg);
  dlg.addEventListener('close', () => dlg.remove());
  dlg.showModal();
}

function clean(dto) {
  const copy = Object.assign({}, dto);
  delete copy.managed;
  return JSON.stringify(copy, null, 2);
}

async function render() {
  for (const b of document.querySelectorAll('#tabs button')) {
    b.classList.toggle('active', b.dataset.tab === state.tab);
  }
  if (state.logsAbort) { state.logsAbort.abort(); state.logsAbort = null; }
  view.replaceChildren(el('p', { class: 'muted', text: 'Loading…' }));
  try {
    switch (state.tab) {
      case 'providers': return await renderProviders();
      case 'connections': return await renderConnections();
      case 'combos': return await renderCombos();
      case 'keys': return await renderKeys();
      case 'usage': return await renderUsage();
      case 'logs': return await renderLogs();
      case 'settings': return await renderSettings();
    }
  } catch (err) {
    setStatus(err.message, 'error');
    view.replaceChildren(el('p', { class: 'error', text: err.message }));
  }
}

function table(headers, rows) {
  const head = el('tr', {}, headers.map((h) => el('th', { text: h })));
  const body = rows.map((cells) => el('tr', {}, cells.map((c) => el('td', {}, c))));
  return el('table', {}, [el('thead', {}, head), el('tbody', {}, body)]);
}

async function renderProviders() {
  const payload = await api('/api/providers');
  const rows = payload.data.map((p) => {
    const actions = [];
    if (p.managed === 'admin') {
      actions.push(el('button', { text: 'Edit', onclick: () => openEditor('Edit provider', clean(p), async (dto) => {
        await api('/api/providers/' + encodeURIComponent(p.id), { method: 'PUT', body: JSON.stringify(dto) });
        await render();
      }) }));
      actions.push(el('button', { class: 'danger', text: 'Delete', onclick: async () => {
        if (!confirm('Delete provider ' + p.id + '?')) return;
        try { await api('/api/providers/' + encodeURIComponent(p.id), { method: 'DELETE' }); await render(); }
        catch (err) { setStatus(err.message, 'error'); }
      } }));
    }
    const test = el('button', { text: 'Test', onclick: async () => {
      test.disabled = true;
      try {
        const result = await api('/api/test-connection', { method: 'POST', body: JSON.stringify({ provider: p.id }) });
        test.replaceWith(badge(result.ok ? 'ok ' + result.status + ' ' + result.latency_ms + 'ms' : 'fail ' + (result.status || result.error), result.ok ? 'ok' : 'bad'));
      } catch (err) {
        test.replaceWith(badge('fail', 'bad'));
        setStatus(err.message, 'error');
      }
    } });
    return [
      p.id,
      p.format || '',
      p.base_url || '',
      (p.models || []).map((m) => m.id).join(', '),
      badge(p.managed, p.managed === 'admin' ? 'admin' : ''),
      [test, ...actions],
    ];
  });
  view.replaceChildren(
    el('h2', { text: 'Providers' }),
    el('div', { class: 'toolbar' }, [
      el('button', { text: 'Add provider', onclick: () => openEditor('New provider',
        '{\n  "id": "my-provider",\n  "base_url": "https://api.example.com/v1",\n  "format": "openai",\n  "auth": "bearer",\n  "models": [{"id": "model-id"}]\n}',
        async (dto) => { await api('/api/providers', { method: 'POST', body: JSON.stringify(dto) }); await render(); }) }),
      el('span', { class: 'muted', text: 'Config-managed providers are read-only. Keys never leave the server.' }),
    ]),
    table(['id', 'format', 'base_url', 'models', 'source', 'actions'], rows),
  );
}

async function renderConnections() {
  const payload = await api('/api/connections');
  const rows = payload.data.map((c) => {
    const actions = [];
    if (c.managed === 'admin') {
      actions.push(el('button', { text: 'Edit', onclick: () => openEditor('Edit connection', clean(c), async (dto) => {
        await api('/api/connections/' + encodeURIComponent(c.id), { method: 'PUT', body: JSON.stringify(dto) });
        await render();
      }) }));
      actions.push(el('button', { class: 'danger', text: 'Delete', onclick: async () => {
        if (!confirm('Delete connection ' + c.id + '?')) return;
        try { await api('/api/connections/' + encodeURIComponent(c.id), { method: 'DELETE' }); await render(); }
        catch (err) { setStatus(err.message, 'error'); }
      } }));
    }
    return [c.id, c.provider, c.base_url || '', c.api_key || '', badge(c.managed, c.managed === 'admin' ? 'admin' : ''), actions];
  });
  view.replaceChildren(
    el('h2', { text: 'Connections' }),
    el('div', { class: 'toolbar' }, [
      el('button', { text: 'Add connection', onclick: () => openEditor('New connection',
        '{\n  "id": "my-connection",\n  "provider": "my-provider",\n  "api_key": "sk-...",\n  "base_url": ""\n}',
        async (dto) => { await api('/api/connections', { method: 'POST', body: JSON.stringify(dto) }); await render(); }) }),
      el('span', { class: 'muted', text: 'api_key is shown as ***; saving *** keeps the stored value.' }),
    ]),
    table(['id', 'provider', 'base_url', 'api_key', 'source', 'actions'], rows),
  );
}

async function renderCombos() {
  const payload = await api('/api/combos');
  const rows = payload.data.map((c) => {
    const actions = [];
    if (c.managed === 'admin') {
      actions.push(el('button', { text: 'Edit', onclick: () => openEditor('Edit combo', clean(c), async (dto) => {
        await api('/api/combos/' + encodeURIComponent(c.name), { method: 'PUT', body: JSON.stringify(dto) });
        await render();
      }) }));
      actions.push(el('button', { class: 'danger', text: 'Delete', onclick: async () => {
        if (!confirm('Delete combo ' + c.name + '?')) return;
        try { await api('/api/combos/' + encodeURIComponent(c.name), { method: 'DELETE' }); await render(); }
        catch (err) { setStatus(err.message, 'error'); }
      } }));
    }
    return [c.name, (c.members || []).join(' → '), c.strategy || 'priority', badge(c.managed, c.managed === 'admin' ? 'admin' : ''), actions];
  });
  view.replaceChildren(
    el('h2', { text: 'Combos' }),
    el('div', { class: 'toolbar' }, [
      el('button', { text: 'Add combo', onclick: () => openEditor('New combo',
        '{\n  "name": "my-combo",\n  "strategy": "priority",\n  "members": ["provider/model"]\n}',
        async (dto) => { await api('/api/combos', { method: 'POST', body: JSON.stringify(dto) }); await render(); }) }),
    ]),
    table(['name', 'members', 'strategy', 'source', 'actions'], rows),
  );
}

async function renderKeys() {
  const payload = await api('/api/keys');
  const rows = payload.keys.map((k) => [
    k.id,
    badge(k.source, 'admin'),
    k.source === 'admin' ? el('button', { class: 'danger', text: 'Delete', onclick: async () => {
      if (!confirm('Delete key ' + k.id + '?')) return;
      try { await api('/api/keys/' + encodeURIComponent(k.id), { method: 'DELETE' }); await render(); }
      catch (err) { setStatus(err.message, 'error'); }
    } }) : el('span', { class: 'muted', text: 'from config' }),
  ]);

  const input = el('input', { type: 'password', placeholder: 'sk-...', autocomplete: 'off' });
  const add = el('button', { text: 'Add key', onclick: async () => {
    if (!input.value) return;
    try {
      await api('/api/keys', { method: 'POST', body: JSON.stringify({ key: input.value }) });
      input.value = '';
      await render();
    } catch (err) { setStatus(err.message, 'error'); }
  } });

  view.replaceChildren(
    el('h2', { text: 'Client keys' }),
    el('div', { class: 'toolbar' }, [input, add, el('span', { class: 'muted', text: 'Keys are shown as a hash prefix, never in full.' })]),
    el('p', { class: 'muted', text: 'config keys: ' + payload.config_keys }),
    table(['id', 'source', 'actions'], rows),
  );
}

async function renderUsage() {
  const selector = el('select');
  for (const g of ['provider', 'model', 'day']) {
    const opt = el('option', { value: g, text: g });
    if (g === state.usageGroup) opt.selected = true;
    selector.appendChild(opt);
  }
  selector.addEventListener('change', () => { state.usageGroup = selector.value; renderUsage(); });
  const payload = await api('/api/usage?group=' + encodeURIComponent(state.usageGroup));

  const rows = payload.data.map((b) => [
    b.key,
    String(b.requests),
    String(b.failures),
    String(b.prompt_tokens),
    String(b.completion_tokens),
    String(b.saved_bytes),
    b.cost_known ? '$' + b.cost_usd.toFixed(6) : 'n/a',
  ]);

  view.replaceChildren(
    el('h2', { text: 'Usage' }),
    el('div', { class: 'toolbar' }, [el('span', { class: 'muted', text: 'group by' }), selector,
      el('span', { class: 'muted', text: 'Ring buffer only; lost on restart.' })]),
    table(['key', 'requests', 'failures', 'prompt', 'completion', 'saved bytes', 'cost'], rows),
  );
}

async function renderLogs() {
  const list = el('div');
  const status = el('span', { class: 'muted', text: 'connecting…' });
  const stop = el('button', { text: 'Stop' });
  const clear = el('button', { text: 'Clear' });
  clear.addEventListener('click', () => list.replaceChildren());

  const controller = new AbortController();
  state.logsAbort = controller;
  stop.addEventListener('click', () => controller.abort());

  const headers = {};
  if (state.token) headers['X-Admin-Token'] = state.token;
  try {
    const res = await fetch('/api/logs/stream', { headers, signal: controller.signal });
    if (!res.ok || !res.body) {
      const text = await res.text();
      throw new Error(text || ('HTTP ' + res.status));
    }
    status.textContent = 'live';
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    (async () => {
      try {
        while (true) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let idx;
          while ((idx = buffer.indexOf('\n\n')) >= 0) {
            const chunk = buffer.slice(0, idx);
            buffer = buffer.slice(idx + 2);
            for (const line of chunk.split('\n')) {
              if (!line.startsWith('data: ')) continue;
              let entry;
              try { entry = JSON.parse(line.slice(6)); } catch (_) { continue; }
              list.prepend(logLine(entry));
              while (list.childElementCount > 200) list.lastChild.remove();
            }
          }
        }
      } catch (err) {
        if (err.name !== 'AbortError') status.textContent = 'stopped: ' + err.message;
      }
    })();
  } catch (err) {
    if (err.name !== 'AbortError') {
      status.textContent = 'error';
      view.replaceChildren(el('h2', { text: 'Logs' }), el('p', { class: 'error', text: err.message }));
      return;
    }
  }

  view.replaceChildren(
    el('h2', { text: 'Live logs' }),
    el('div', { class: 'toolbar' }, [status, stop, clear]),
    list,
  );
}

function logLine(e) {
  const time = new Date(e.time).toLocaleTimeString();
  const provider = e.provider ? e.provider + '/' + e.model : e.requested;
  const line = [
    e.status,
    provider,
    e.duration_ms + 'ms',
    e.prompt_tokens + '/' + e.completion_tokens + ' tok',
    e.saved_bytes ? e.saved_bytes + ' saved' : '',
    e.cost_known ? '$' + e.cost_usd.toFixed(6) : '',
    e.error || '',
  ].filter(Boolean).join('  ');
  const row = el('div', { class: 'log-line' }, [el('span', { class: 't', text: time }), line]);
  if (e.status >= 400) row.classList.add('error');
  return row;
}

async function renderSettings() {
  const [ready, version] = await Promise.all([
    fetch('/readyz').then((r) => r.json()),
    fetch('/version').then((r) => r.json()),
  ]);
  const info = {
    version,
    ready,
    ui: 'embedded, no runtime assets',
    admin_auth: state.token ? 'token set in this browser' : 'no token stored',
  };
  view.replaceChildren(
    el('h2', { text: 'Settings' }),
    el('p', { class: 'muted', text: 'Configuration stays in config.yaml and admin.yaml; this page only calls the admin API.' }),
    el('pre', { text: JSON.stringify(info, null, 2) }),
    el('h3', { text: 'Raw endpoints' }),
    el('ul', {}, [
      el('li', {}, [el('a', { href: '/metrics', text: '/metrics' })]),
      el('li', {}, [el('a', { href: '/usage', text: '/usage' })]),
      el('li', {}, [el('a', { href: '/readyz', text: '/readyz' })]),
    ]),
  );
}

document.getElementById('tabs').addEventListener('click', (event) => {
  const button = event.target.closest('button[data-tab]');
  if (!button) return;
  state.tab = button.dataset.tab;
  render();
});

document.getElementById('save-token').addEventListener('click', () => {
  state.token = tokenEl.value.trim();
  if (state.token) localStorage.setItem('lgllm.admin.token', state.token);
  else localStorage.removeItem('lgllm.admin.token');
  setStatus(state.token ? 'token saved' : 'token cleared', 'ok');
  render();
});

fetch('/version').then((r) => r.json()).then((v) => {
  document.getElementById('version').textContent = v.version || '';
}).catch(() => {});

render();