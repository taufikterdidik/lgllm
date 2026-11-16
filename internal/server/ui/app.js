'use strict';

const state = {
  token: localStorage.getItem('lgllm.admin.token') || '',
  tab: 'providers',
  logsAbort: null,
  perfTimer: null,
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

const FORMAT_OPTIONS = [
  { value: 'openai', label: 'OpenAI Chat Completions' },
  { value: 'anthropic', label: 'Anthropic Messages' },
  { value: 'gemini', label: 'Gemini generateContent' },
  { value: 'openai-responses', label: 'OpenAI Responses' },
  { value: 'ollama', label: 'Ollama native' },
];

const AUTH_OPTIONS = [
  { value: 'bearer', label: 'Bearer token' },
  { value: 'x-api-key', label: 'x-api-key header' },
  { value: 'query', label: 'Query parameter' },
  { value: 'none', label: 'No authentication' },
];

let formSequence = 0;

function formField(label, control, hint) {
  return el('label', { class: 'field' }, [
    el('span', { class: 'field-label', text: label }),
    control,
    hint ? el('span', { class: 'field-hint', text: hint }) : null,
  ]);
}

function textInput(value = '', attrs = {}) {
  const input = el('input', Object.assign({ type: 'text' }, attrs));
  input.value = value === null || value === undefined ? '' : value;
  return input;
}

function numberInput(value = 0, attrs = {}) {
  const input = el('input', Object.assign({ type: 'number', min: '0' }, attrs));
  input.value = value === null || value === undefined ? '' : value;
  return input;
}

function selectInput(options, value) {
  const select = el('select');
  for (const option of options) {
    const item = el('option', { value: option.value, text: option.label });
    if (String(option.value) === String(value)) item.selected = true;
    select.appendChild(item);
  }
  return select;
}

function checkboxInput(label, checked) {
  const input = el('input', { type: 'checkbox' });
  input.checked = Boolean(checked);
  return { input, node: el('label', { class: 'check' }, [input, el('span', { text: label })]) };
}

function formSection(title, children, hint) {
  return el('fieldset', { class: 'form-section' }, [
    el('legend', { text: title }),
    hint ? el('p', { class: 'field-hint', text: hint }) : null,
    ...children,
  ]);
}

function openForm(title, content, onSave, saveText = 'Save') {
  const dlg = el('dialog', { class: 'form-dialog' });
  const error = el('div', { class: 'error' });
  const save = el('button', { type: 'button', class: 'primary', text: saveText });
  const cancel = el('button', { type: 'button', text: 'Cancel' });
  let busy = false;

  const submit = async () => {
    if (busy) return;
    busy = true;
    save.disabled = true;
    error.textContent = '';
    try {
      await onSave();
      dlg.close();
    } catch (err) {
      error.textContent = err.message;
    } finally {
      busy = false;
      save.disabled = false;
    }
  };

  save.addEventListener('click', submit);
  cancel.addEventListener('click', () => dlg.close());
  const form = el('form', { class: 'modal-form' }, [
    ...content,
    error,
    el('menu', {}, [cancel, save]),
  ]);
  form.addEventListener('submit', (event) => {
    event.preventDefault();
    submit();
  });
  dlg.append(el('h3', { text: title }), form);
  document.body.appendChild(dlg);
  dlg.addEventListener('close', () => dlg.remove());
  dlg.showModal();
  const first = form.querySelector('input:not([type=checkbox]), select, textarea');
  if (first) first.focus();
  return { dlg, error, close: () => dlg.close() };
}

function openInfo(title, content) {
  const dlg = el('dialog', { class: 'form-dialog' });
  const close = el('button', { type: 'button', class: 'primary', text: 'Done' });
  close.addEventListener('click', () => dlg.close());
  dlg.append(el('h3', { text: title }), el('div', { class: 'modal-form' }, [
    ...content,
    el('menu', {}, [close]),
  ]));
  document.body.appendChild(dlg);
  dlg.addEventListener('close', () => dlg.remove());
  dlg.showModal();
}

function copyButton(text) {
  const button = el('button', { type: 'button', text: 'Copy' });
  button.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text);
    } catch (_) {
      const area = el('textarea', { class: 'copy-fallback' });
      area.value = text;
      document.body.appendChild(area);
      area.select();
      document.execCommand('copy');
      area.remove();
    }
    button.textContent = 'Copied';
    setStatus('copied to clipboard', 'ok');
  });
  return button;
}

function keyValueEditor(initial) {
  const list = el('div', { class: 'rows' });
  const addRow = (key = '', value = '') => {
    const row = el('div', { class: 'row-editor' });
    const keyInput = textInput(key, { placeholder: 'header-name' });
    const valueInput = textInput(value, { placeholder: 'value (*** keeps a stored secret)' });
    const remove = el('button', { type: 'button', class: 'danger small', text: '×' });
    remove.addEventListener('click', () => row.remove());
    row.append(keyInput, valueInput, remove);
    list.appendChild(row);
    return row;
  };
  const replace = (values) => {
    list.replaceChildren();
    for (const [key, value] of Object.entries(values || {})) addRow(key, value);
  };
  replace(initial);
  const add = el('button', { type: 'button', class: 'small', text: '+ header' });
  add.addEventListener('click', () => addRow());
  const node = el('div', {}, [list, add]);
  return {
    node,
    replace,
    values() {
      const out = {};
      for (const row of list.children) {
        const inputs = row.querySelectorAll('input');
        const key = inputs[0].value.trim();
        if (key) out[key] = inputs[1].value;
      }
      return out;
    },
  };
}

function modelEditor(initial) {
  const list = el('div', { class: 'rows' });
  const addRow = (model = {}) => {
    const row = el('div', { class: 'model-row' });
    const id = textInput(model.id || '', { placeholder: 'model-id' });
    const name = textInput(model.name || '', { placeholder: 'display name (optional)' });
    const context = numberInput(model.context || '', { placeholder: 'context' });
    const capabilities = Object.keys(model.capabilities || {}).filter((k) => model.capabilities[k]).join(',');
    const caps = textInput(capabilities, { placeholder: 'tools, vision (optional)' });
    const remove = el('button', { type: 'button', class: 'danger small', text: '×' });
    remove.addEventListener('click', () => row.remove());
    row.append(id, name, context, caps, remove);
    list.appendChild(row);
    return row;
  };
  const replace = (models) => {
    list.replaceChildren();
    for (const model of models || []) addRow(model);
  };
  replace(initial);
  const add = el('button', { type: 'button', class: 'small', text: '+ model' });
  add.addEventListener('click', () => addRow());
  const node = el('div', {}, [list, add]);
  return {
    node,
    replace,
    values() {
      const out = [];
      for (const row of list.children) {
        const inputs = row.querySelectorAll('input');
        const id = inputs[0].value.trim();
        if (!id) continue;
        const model = { id };
        const name = inputs[1].value.trim();
        if (name) model.name = name;
        const context = Number(inputs[2].value || 0);
        if (context > 0) model.context = context;
        const caps = inputs[3].value.split(/[,\s]+/).map((s) => s.trim()).filter(Boolean);
        if (caps.length) {
          model.capabilities = {};
          for (const cap of caps) model.capabilities[cap] = true;
        }
        out.push(model);
      }
      return out;
    },
  };
}

function providerTargets(providers) {
  const out = [];
  const seen = new Set();
  for (const provider of providers || []) {
    const prefix = provider.alias || provider.id;
    for (const model of provider.models || []) {
      const ref = prefix + '/' + model.id;
      if (!seen.has(ref)) {
        seen.add(ref);
        out.push(ref);
      }
    }
  }
  out.sort();
  return out;
}

function memberEditor(initial, providers) {
  const list = el('div', { class: 'rows' });
  const datalistID = 'targets-' + (++formSequence);
  const datalist = el('datalist', { id: datalistID });
  for (const ref of providerTargets(providers)) {
    datalist.appendChild(el('option', { value: ref }));
  }

  const move = (row, direction) => {
    const sibling = direction < 0 ? row.previousElementSibling : row.nextElementSibling;
    if (!sibling) return;
    list.insertBefore(row, direction < 0 ? sibling : sibling.nextElementSibling);
  };

  const addRow = (value = '') => {
    const row = el('div', { class: 'member-row' });
    const input = textInput(value, { list: datalistID, placeholder: 'provider/model or alias' });
    const up = el('button', { type: 'button', class: 'small', text: '↑' });
    const down = el('button', { type: 'button', class: 'small', text: '↓' });
    const remove = el('button', { type: 'button', class: 'danger small', text: '×' });
    up.addEventListener('click', () => move(row, -1));
    down.addEventListener('click', () => move(row, 1));
    remove.addEventListener('click', () => row.remove());
    row.append(input, up, down, remove);
    list.appendChild(row);
    return row;
  };

  const replace = (members) => {
    list.replaceChildren();
    for (const member of members || []) addRow(member);
    if (!list.children.length) addRow('');
  };
  replace(initial);

  const free = textInput('', { list: datalistID, placeholder: 'provider/model or alias' });
  const add = el('button', { type: 'button', class: 'small', text: '+ member' });
  add.addEventListener('click', () => {
    if (free.value.trim()) {
      addRow(free.value.trim());
      free.value = '';
    }
  });
  const node = el('div', {}, [datalist, list, el('div', { class: 'row-add' }, [free, add])]);
  return {
    node,
    values() {
      const out = [];
      for (const row of list.children) {
        const value = row.querySelector('input').value.trim();
        if (value) out.push(value);
      }
      return out;
    },
  };
}

async function render() {
  for (const b of document.querySelectorAll('#tabs button')) {
    b.classList.toggle('active', b.dataset.tab === state.tab);
  }
  if (state.logsAbort) { state.logsAbort.abort(); state.logsAbort = null; }
  if (state.perfTimer) { clearInterval(state.perfTimer); state.perfTimer = null; }
  view.replaceChildren(el('p', { class: 'muted', text: 'Loading…' }));
  try {
    switch (state.tab) {
      case 'providers': return await renderProviders();
      case 'connections': return await renderConnections();
      case 'combos': return await renderCombos();
      case 'keys': return await renderKeys();
      case 'usage': return await renderUsage();
      case 'performance': return await renderPerformance();
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

function providerLabel(provider) {
  if (!provider) return '';
  const name = provider.name || provider.id;
  const refs = [provider.id];
  if (provider.alias) refs.push(provider.alias);
  return name + ' (' + refs.join(' / ') + ')';
}

function openProviderForm(existing, catalog, providers) {
  const isNew = !existing;
  const catalogByID = new Map((catalog || []).map((entry) => [entry.id, entry]));
  const source = selectInput([
    { value: 'catalog', label: 'Built-in catalog' },
    { value: 'custom', label: 'Custom provider' },
  ], existing && existing.managed !== 'catalog' ? 'custom' : 'catalog');
  const catalogSelect = selectInput(
    [{ value: '', label: 'Choose a catalog provider…' }].concat(
      (catalog || []).map((entry) => ({ value: entry.id, label: entry.name + ' (' + entry.id + ')' }))),
    existing && existing.managed === 'catalog' ? existing.id : '',
  );
  const id = textInput(existing ? existing.id : '');
  const alias = textInput(existing ? existing.alias || '' : '');
  const base = textInput(existing ? existing.base_url || '' : '');
  const format = selectInput(FORMAT_OPTIONS, existing ? existing.format || 'openai' : 'openai');
  const auth = selectInput(AUTH_OPTIONS, existing ? existing.auth || 'bearer' : 'bearer');
  const authParam = textInput(existing ? existing.auth_param || '' : '', { placeholder: 'api_key' });
  const timeout = numberInput(existing && existing.timeout_ms ? existing.timeout_ms : '', { placeholder: '0 = default' });
  const sessionHeader = textInput(existing ? existing.session_header || '' : '');
  const headers = keyValueEditor(existing ? existing.headers || {} : {});
  const models = modelEditor(existing ? existing.models || [] : []);
  const keyHint = el('span', { class: 'field-hint' });
  const authParamField = formField('Auth parameter', authParam, 'Used when auth is "query".');

  const syncAuthParam = () => {
    authParamField.style.display = auth.value === 'query' ? '' : 'none';
  };
  auth.addEventListener('change', syncAuthParam);
  syncAuthParam();

  const fillFromCatalog = (entry) => {
    if (!entry) return;
    id.value = entry.id || '';
    alias.value = entry.alias || '';
    base.value = entry.base_url || '';
    format.value = entry.format || 'openai';
    auth.value = entry.auth || 'bearer';
    authParam.value = entry.auth_param || '';
    sessionHeader.value = entry.session_header || '';
    headers.replace(entry.headers || {});
    models.replace((entry.models || []).map((model) => ({
      id: model.id || model,
      context: model.context || 0,
    })));
    keyHint.textContent = entry.key_env ? 'Conventional environment variable: ' + entry.key_env : '';
    syncAuthParam();
  };

  const clearForm = () => {
    id.value = '';
    alias.value = '';
    base.value = '';
    format.value = 'openai';
    auth.value = 'bearer';
    authParam.value = '';
    sessionHeader.value = '';
    headers.replace({});
    models.replace([]);
    keyHint.textContent = '';
    syncAuthParam();
  };

  if (existing && existing.managed === 'catalog') {
    fillFromCatalog(catalogByID.get(existing.id));
  } else if (!existing && catalogSelect.value) {
    fillFromCatalog(catalogByID.get(catalogSelect.value));
  }
  if (existing) {
    source.disabled = true;
    catalogSelect.disabled = true;
  } else {
    catalogSelect.addEventListener('change', () => {
      if (source.value === 'catalog') fillFromCatalog(catalogByID.get(catalogSelect.value));
    });
    source.addEventListener('change', () => {
      const catalogSource = source.value === 'catalog';
      catalogSelect.disabled = !catalogSource;
      if (catalogSource) fillFromCatalog(catalogByID.get(catalogSelect.value));
      else clearForm();
    });
  }

  const content = [
    formSection('Provider', [
      el('div', { class: 'form-grid' }, [
        formField('Source', source),
        formField('Catalog entry', catalogSelect),
      ]),
      el('div', { class: 'form-grid' }, [
        formField('Provider id', id, 'Used as the model prefix and connection target.'),
        formField('Alias', alias, 'Optional short prefix.'),
      ]),
    ]),
    formSection('Endpoint', [
      formField('Base URL', base, 'API root, for example https://api.example.com/v1.'),
      el('div', { class: 'form-grid' }, [
        formField('Format', format),
        formField('Auth', auth),
        formField('Timeout (ms)', timeout),
      ]),
      authParamField,
      formField('Session header', sessionHeader, 'Optional header that receives a stable conversation id.'),
      keyHint,
    ]),
    formSection('Headers', [headers.node], 'Secret-looking values are redacted as ***; leaving *** keeps the stored value.'),
    formSection('Models', [models.node], 'These are hints for /v1/models; generation still forwards any model id.'),
  ];

  const title = existing
    ? (existing.managed === 'catalog' ? 'Customize built-in provider' : 'Edit provider')
    : 'Add provider';

  openForm(title, content, async () => {
    const dto = {
      id: id.value.trim(),
      alias: alias.value.trim(),
      base_url: base.value.trim(),
      format: format.value,
      auth: auth.value,
      session_header: sessionHeader.value.trim(),
      headers: headers.values(),
      models: models.values(),
    };
    if (authParam.value.trim()) dto.auth_param = authParam.value.trim();
    const timeoutMS = Number(timeout.value || 0);
    if (timeoutMS > 0) dto.timeout_ms = timeoutMS;
    if (!dto.id) throw new Error('Provider id is required');
    if (!dto.base_url) throw new Error('Base URL is required');

    if (isNew || existing.managed === 'catalog') {
      await api('/api/providers', { method: 'POST', body: JSON.stringify(dto) });
    } else {
      await api('/api/providers/' + encodeURIComponent(existing.id), {
        method: 'PUT', body: JSON.stringify(dto),
      });
    }
    await render();
    if (isNew && confirm('Provider saved. Add a connection now?')) {
      const list = await api('/api/providers?all=1');
      setTimeout(() => openConnectionForm(null, list.data, dto.id), 0);
    }
  }, isNew ? 'Add provider' : 'Save provider');
}

function suggestedConnectionID(provider) {
  return (provider || 'connection') + '-main';
}

function openConnectionForm(existing, providers, presetProvider) {
  if (!providers || !providers.length) {
    setStatus('Add a provider first', 'error');
    return;
  }
  const providerSelect = selectInput(
    providers.map((provider) => ({ value: provider.id, label: providerLabel(provider) })),
    existing ? existing.provider : (presetProvider || providers[0].id),
  );
  const id = textInput(existing ? existing.id : suggestedConnectionID(providerSelect.value));
  let autoID = !existing;
  providerSelect.addEventListener('change', () => {
    if (autoID) id.value = suggestedConnectionID(providerSelect.value);
  });
  id.addEventListener('input', () => { autoID = false; });
  const key = textInput('', {
    type: 'password', autocomplete: 'off',
    placeholder: existing ? '*** unchanged' : 'sk-...',
  });
  const base = textInput(existing ? existing.base_url || '' : '', { placeholder: 'inherit provider base URL' });
  const disabled = checkboxInput('Disabled', existing ? existing.disabled : false);
  const keyHint = existing
    ? 'Leave blank to keep the stored key. It is never returned by the API.'
    : 'Stored in admin.yaml; encrypted when a master key is configured.';

  const content = [
    formSection('Connection', [
      formField('Provider', providerSelect),
      formField('Connection id', id),
      formField('API key', key, keyHint),
      formField('Base URL override', base, 'Optional; useful for a regional gateway.'),
      disabled.node,
    ]),
  ];

  openForm(existing ? 'Edit connection' : 'Add connection', content, async () => {
    const dto = {
      id: id.value.trim(),
      provider: providerSelect.value,
      base_url: base.value.trim(),
      disabled: disabled.input.checked,
    };
    if (!dto.id) throw new Error('Connection id is required');
    if (key.value) dto.api_key = key.value;

    if (existing) {
      await api('/api/connections/' + encodeURIComponent(existing.id), {
        method: 'PUT', body: JSON.stringify(dto),
      });
    } else {
      await api('/api/connections', { method: 'POST', body: JSON.stringify(dto) });
    }
    await render();
  }, existing ? 'Save connection' : 'Add connection');
}

function openComboForm(existing, providers) {
  const name = textInput(existing ? existing.name : '');
  if (existing) name.disabled = true;
  const strategy = selectInput([
    { value: 'priority', label: 'Priority (try in order)' },
    { value: 'round-robin', label: 'Round-robin' },
  ], existing ? existing.strategy || 'priority' : 'priority');
  const tokenSaver = selectInput([
    { value: '', label: 'Inherit global setting' },
    { value: 'true', label: 'On' },
    { value: 'false', label: 'Off' },
  ], existing && existing.token_saver !== undefined && existing.token_saver !== null
    ? String(existing.token_saver) : '');
  const members = memberEditor(existing ? existing.members || [] : [], providers);

  const content = [
    formSection('Combo', [
      formField('Name', name, existing ? 'Combo names are fixed; create a new combo to rename.' : ''),
      formField('Strategy', strategy),
      formField('Token saver', tokenSaver),
    ]),
    formSection('Members', [members.node], 'Order matters for priority. Pick a model or type an alias/combo name.'),
  ];

  openForm(existing ? 'Edit combo' : 'Add combo', content, async () => {
    const list = members.values();
    if (!name.value.trim()) throw new Error('Combo name is required');
    if (!list.length) throw new Error('Add at least one member');
    const dto = { name: name.value.trim(), members: list, strategy: strategy.value };
    if (tokenSaver.value !== '') dto.token_saver = tokenSaver.value === 'true';

    if (existing) {
      await api('/api/combos/' + encodeURIComponent(existing.name), {
        method: 'PUT', body: JSON.stringify(dto),
      });
    } else {
      await api('/api/combos', { method: 'POST', body: JSON.stringify(dto) });
    }
    await render();
  }, existing ? 'Save combo' : 'Add combo');
}

function openKeyForm() {
  const name = textInput('', { placeholder: 'laptop, ci, phone…' });
  const key = textInput('', { type: 'password', autocomplete: 'off', placeholder: 'sk-...' });
  openForm('Add existing client key', [
    formSection('Client key', [
      formField('Name', name, 'Optional label shown in the key list.'),
      formField('API key', key, 'At least 8 characters. Prefer Generate key for a new random key.'),
    ]),
  ], async () => {
    if (!key.value.trim()) throw new Error('API key is required');
    await api('/api/keys', {
      method: 'POST',
      body: JSON.stringify({ key: key.value.trim(), name: name.value.trim() }),
    });
    await render();
  }, 'Add key');
}

function openGenerateKeyForm() {
  const name = textInput('', { placeholder: 'laptop, ci, phone…' });
  openForm('Generate client key', [
    formSection('Client key', [
      formField('Name', name, 'Optional label shown in the key list.'),
    ]),
  ], async () => {
    const created = await api('/api/keys/generate', {
      method: 'POST',
      body: JSON.stringify({ name: name.value.trim() }),
    });
    await render();
    setTimeout(() => showGeneratedKey(created), 0);
  }, 'Generate key');
}

function openRenameKeyForm(key) {
  const name = textInput(key.name || '');
  openForm('Rename client key', [
    formSection('Client key', [
      formField('Name', name, 'Leave empty to clear the label.'),
    ]),
  ], async () => {
    await api('/api/keys/' + encodeURIComponent(key.id), {
      method: 'PUT',
      body: JSON.stringify({ name: name.value.trim() }),
    });
    await render();
  }, 'Save name');
}

function showGeneratedKey(created) {
  const output = textInput(created.key, { readonly: 'readonly', class: 'key-output' });
  const content = [
    el('p', { class: 'warn', text: 'Copy this key now. It is shown once and is never returned by GET /api/keys.' }),
  ];
  if (created.name) content.push(el('p', { class: 'muted', text: 'Name: ' + created.name }));
  content.push(output, el('div', { class: 'toolbar' }, [copyButton(created.key)]));
  openInfo('Client key generated', content);
}

function testConnection(provider, connection, model) {
  const button = el('button', { class: 'small', text: 'Test' });
  button.addEventListener('click', async () => {
    button.disabled = true;
    try {
      const body = { provider };
      if (connection) body.connection = connection;
      if (model) body.model = model;
      const result = await api('/api/test-connection', { method: 'POST', body: JSON.stringify(body) });
      button.replaceWith(badge(
        result.ok ? 'ok ' + result.status + ' ' + result.latency_ms + 'ms' : 'fail ' + (result.status || result.error),
        result.ok ? 'ok' : 'bad',
      ));
    } catch (err) {
      button.replaceWith(badge('fail', 'bad'));
      setStatus(err.message, 'error');
    }
  });
  return button;
}

async function renderProviders() {
  const [payload, catalog] = await Promise.all([
    api('/api/providers?all=1'),
    api('/api/catalog'),
  ]);
  const rows = payload.data.map((p) => {
    const actions = [];
    actions.push(el('button', {
      class: 'small', text: 'Add connection',
      onclick: () => openConnectionForm(null, payload.data, p.id),
    }));
    if (p.managed !== 'config') {
      actions.push(el('button', {
        class: 'small', text: p.managed === 'catalog' ? 'Customize' : 'Edit',
        onclick: () => openProviderForm(p, catalog.data, payload.data),
      }));
    }
    if (p.connected) actions.push(testConnection(p.id));
    if (p.managed === 'admin') {
      actions.push(el('button', {
        class: 'danger small', text: 'Delete',
        onclick: async () => {
          if (!confirm('Delete provider ' + p.id + '?')) return;
          try {
            await api('/api/providers/' + encodeURIComponent(p.id), { method: 'DELETE' });
            await render();
          } catch (err) { setStatus(err.message, 'error'); }
        },
      }));
    }

    const name = el('div', {}, [
      el('strong', { text: p.name || p.id }),
      el('div', { class: 'muted small', text: p.id + (p.alias ? ' / ' + p.alias : '') }),
    ]);
    const connection = p.connected
      ? badge(p.connections + ' connected', 'ok')
      : badge('no connection', '');
    return [
      name,
      p.format || '',
      String((p.models || []).length) + ' models',
      connection,
      badge(p.managed, p.managed === 'admin' ? 'admin' : ''),
      actions,
    ];
  });

  view.replaceChildren(
    el('h2', { text: 'Providers' }),
    el('div', { class: 'toolbar' }, [
      el('button', {
        class: 'primary', text: 'Add provider',
        onclick: () => openProviderForm(null, catalog.data, payload.data),
      }),
      el('span', { class: 'muted', text: 'Choose a catalog provider or define a custom endpoint. Config-managed entries stay read-only.' }),
    ]),
    table(['provider', 'format', 'models', 'connections', 'source', 'actions'], rows),
  );
}

async function renderConnections() {
  const [payload, providers] = await Promise.all([
    api('/api/connections'),
    api('/api/providers?all=1'),
  ]);
  const rows = payload.data.map((c) => {
    const actions = [];
    if (c.managed === 'admin') {
      actions.push(el('button', {
        class: 'small', text: 'Edit',
        onclick: () => openConnectionForm(c, providers.data),
      }));
      actions.push(el('button', {
        class: 'danger small', text: 'Delete',
        onclick: async () => {
          if (!confirm('Delete connection ' + c.id + '?')) return;
          try {
            await api('/api/connections/' + encodeURIComponent(c.id), { method: 'DELETE' });
            await render();
          } catch (err) { setStatus(err.message, 'error'); }
        },
      }));
    }
    if (!c.disabled) actions.push(testConnection(c.provider, c.id));
    return [
      c.id,
      c.provider,
      c.base_url ? c.base_url : el('span', { class: 'muted', text: 'inherit' }),
      c.api_key || '—',
      c.disabled ? badge('disabled', 'bad') : badge('enabled', 'ok'),
      badge(c.managed, c.managed === 'admin' ? 'admin' : ''),
      actions,
    ];
  });

  view.replaceChildren(
    el('h2', { text: 'Connections' }),
    el('div', { class: 'toolbar' }, [
      el('button', {
        class: 'primary', text: 'Add connection',
        onclick: () => openConnectionForm(null, providers.data),
      }),
      el('span', { class: 'muted', text: 'API keys are shown as ***; leaving the field blank keeps the stored value.' }),
    ]),
    table(['id', 'provider', 'base_url', 'api_key', 'status', 'source', 'actions'], rows),
  );
}

async function renderCombos() {
  const [payload, providers] = await Promise.all([
    api('/api/combos'),
    api('/api/providers?all=1'),
  ]);
  const rows = payload.data.map((c) => {
    const actions = [];
    if (c.managed === 'admin') {
      actions.push(el('button', {
        class: 'small', text: 'Edit',
        onclick: () => openComboForm(c, providers.data),
      }));
      actions.push(el('button', {
        class: 'danger small', text: 'Delete',
        onclick: async () => {
          if (!confirm('Delete combo ' + c.name + '?')) return;
          try {
            await api('/api/combos/' + encodeURIComponent(c.name), { method: 'DELETE' });
            await render();
          } catch (err) { setStatus(err.message, 'error'); }
        },
      }));
    }
    const saver = c.token_saver === undefined || c.token_saver === null
      ? 'inherit' : (c.token_saver ? 'on' : 'off');
    const context = c.context_known ? fmtContext(c.context_length) : 'unknown';
    return [
      c.name,
      (c.members || []).join(' → '),
      c.strategy || 'priority',
      context,
      saver,
      badge(c.managed, c.managed === 'admin' ? 'admin' : ''),
      actions,
    ];
  });

  view.replaceChildren(
    el('h2', { text: 'Combos' }),
    el('div', { class: 'toolbar' }, [
      el('button', {
        class: 'primary', text: 'Add combo',
        onclick: () => openComboForm(null, providers.data),
      }),
      el('span', { class: 'muted', text: 'A combo is a named list of models tried in order or round-robin.' }),
    ]),
    table(['name', 'members', 'strategy', 'ctx min', 'token saver', 'source', 'actions'], rows),
  );
}

async function renderKeys() {
  const payload = await api('/api/keys');
  const rows = payload.keys.map((k) => {
    const actions = [];
    if (k.source === 'admin') {
      actions.push(el('button', {
        class: 'small', text: 'Rename',
        onclick: () => openRenameKeyForm(k),
      }));
      actions.push(el('button', {
        class: 'danger small', text: 'Delete',
        onclick: async () => {
          if (!confirm('Delete key ' + (k.name || k.id) + '?')) return;
          try {
            await api('/api/keys/' + encodeURIComponent(k.id), { method: 'DELETE' });
            await render();
          } catch (err) { setStatus(err.message, 'error'); }
        },
      }));
    } else {
      actions.push(el('span', { class: 'muted', text: 'from config' }));
    }
    return [
      k.name ? el('strong', { text: k.name }) : el('span', { class: 'muted', text: 'unnamed' }),
      k.id,
      badge(k.source, 'admin'),
      actions,
    ];
  });

  const generate = el('button', {
    class: 'primary', text: 'Generate key',
    onclick: openGenerateKeyForm,
  });
  const add = el('button', { text: 'Add existing key', onclick: openKeyForm });

  view.replaceChildren(
    el('h2', { text: 'Client keys' }),
    el('div', { class: 'toolbar' }, [
      generate,
      add,
      el('span', { class: 'muted', text: 'Names are labels only; generated keys are shown once and listed by hash prefix.' }),
    ]),
    el('p', { class: 'muted', text: 'config keys: ' + payload.config_keys }),
    table(['name', 'id', 'source', 'actions'], rows),
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

function fmtContext(tokens) {
  if (!tokens) return 'unknown';
  if (tokens >= 1000000) return (tokens / 1000000) + 'M';
  if (tokens >= 1000) return Math.round(tokens / 1000) + 'k';
  return String(tokens);
}

function fmtMs(value) {
  if (value === null || value === undefined) return 'n/a';
  if (value >= 1000) return (value / 1000).toFixed(2) + ' s';
  return (value < 10 ? value.toFixed(1) : Math.round(value)) + ' ms';
}

function fmtTps(mode) {
  if (!mode || mode.avg_tps === null || mode.avg_tps === undefined) return 'n/a';
  return mode.avg_tps.toFixed(1) + (mode.tps_estimated ? ' est.' : '');
}

// pickPerfMode chooses the mode whose TPS is most useful. When no provider sent
// usage, it falls back to whichever mode has samples so TTFT/latency still
// render instead of showing an empty row.
function pickPerfMode(m) {
  const hasTps = (s) => s && s.avg_tps !== null && s.avg_tps !== undefined;
  if (hasTps(m.stream)) return m.stream;
  if (hasTps(m.non_stream)) return m.non_stream;
  return m.stream.samples > 0 ? m.stream : m.non_stream;
}

function perfModes(m) {
  const modes = [];
  if (m.stream.samples > 0) modes.push('stream ' + m.stream.samples);
  if (m.non_stream.samples > 0) modes.push('non-stream ' + m.non_stream.samples);
  return modes.length ? modes.join(' · ') : '—';
}

function tpsCell(mode) {
  const node = el('span', { text: fmtTps(mode) });
  if (mode.low_sample) {
    node.appendChild(document.createTextNode(' '));
    node.appendChild(badge('low'));
  }
  return node;
}

const svgNS = 'http://www.w3.org/2000/svg';

function svgEl(tag, attrs = {}, children = []) {
  const node = document.createElementNS(svgNS, tag);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  for (const child of [].concat(children)) {
    node.appendChild(typeof child === 'string' ? document.createTextNode(child) : child);
  }
  return node;
}

// sparkline is a tiny TPS trend for the table. Failed samples are marked at the
// baseline so a row with many failures is distinguishable from a fast model.
function sparkline(samples, key) {
  samples = samples || [];
  const points = samples.map((s, i) => ({ s, i }))
    .filter((p) => p.s.ok && typeof p.s[key] === 'number' && isFinite(p.s[key]));
  if (!points.length) return el('span', { class: 'muted', text: '—' });

  const width = 110, height = 26, pad = 3;
  const values = points.map((p) => p.s[key]);
  const min = Math.min(...values), max = Math.max(...values);
  const span = max - min;
  const x = (i) => samples.length <= 1 ? width / 2 : pad + (i / (samples.length - 1)) * (width - 2 * pad);
  const y = (v) => height - pad - (span === 0 ? 0 : (v - min) / span) * (height - 2 * pad);

  const children = [];
  const title = samples.map((s) => {
    const t = new Date(s.at).toLocaleTimeString();
    if (!s.ok) return t + ' · failed';
    const v = s[key];
    return t + ' · ' + (typeof v === 'number' && isFinite(v) ? v.toFixed(1) : 'n/a');
  }).join('\n');
  children.push(svgEl('title', {}, [title]));
  samples.forEach((s, i) => {
    if (!s.ok) children.push(svgEl('circle', {
      cx: x(i), cy: height - 2, r: '1.5', fill: '#f2777a',
    }));
  });
  children.push(svgEl('polyline', {
    points: points.map((p) => x(p.i) + ',' + y(p.s[key])).join(' '),
    fill: 'none', stroke: '#6ea8fe', 'stroke-width': '1.5',
    'stroke-linecap': 'round', 'stroke-linejoin': 'round',
  }));
  points.forEach((p) => {
    children.push(svgEl('circle', {
      cx: x(p.i), cy: y(p.s[key]), r: '1.7', fill: '#6ea8fe',
    }));
  });
  return svgEl('svg', {
    viewBox: '0 0 ' + width + ' ' + height, class: 'spark', role: 'img',
  }, children);
}

function niceMax(v) {
  if (!(v > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / p;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return step * p;
}

// lineChart draws one or more series over the attempt ring, including red
// markers for failed attempts. It is deliberately plain SVG: no chart library,
// no build step, and the same code works for TTFT, latency and TPS.
function lineChart(samples, specs, unit) {
  samples = samples || [];
  const series = specs.map((spec) => ({
    spec,
    points: samples.map((s, i) => ({ s, i }))
      .filter((p) => p.s.ok && typeof p.s[spec.key] === 'number' && isFinite(p.s[spec.key])),
  })).filter((s) => s.points.length);
  if (!series.length) return el('p', { class: 'muted', text: 'n/a' });

  const width = 760, height = 150;
  const left = 46, right = 10, top = 12, bottom = 22;
  const plotW = width - left - right, plotH = height - top - bottom;
  const max = niceMax(Math.max(...series.flatMap((s) => s.points.map((p) => p.s[s.spec.key]))));
  const x = (i) => samples.length <= 1 ? left + plotW / 2 : left + (i / (samples.length - 1)) * plotW;
  const y = (v) => top + plotH - (v / max) * plotH;

  const children = [];
  for (let i = 0; i <= 2; i++) {
    const v = max * i / 2;
    const yy = y(v);
    children.push(svgEl('line', {
      x1: left, x2: width - right, y1: yy, y2: yy,
      stroke: '#262c36', 'stroke-width': '1',
    }));
    children.push(svgEl('text', {
      x: left - 6, y: yy + 3, 'text-anchor': 'end', fill: '#8b93a3', 'font-size': '10',
    }, [unit === 'ms' ? Math.round(v) + 'ms' : v.toFixed(1)]));
  }
  samples.forEach((s, i) => {
    if (s.ok) return;
    const xx = x(i);
    children.push(svgEl('line', {
      x1: xx, x2: xx, y1: top, y2: top + plotH,
      stroke: '#f2777a', 'stroke-width': '1', 'stroke-dasharray': '2 3', opacity: '0.7',
    }, [svgEl('title', {}, [new Date(s.at).toLocaleTimeString() + ' · failed'])]));
  });
  for (const { spec, points } of series) {
    children.push(svgEl('polyline', {
      points: points.map((p) => x(p.i) + ',' + y(p.s[spec.key])).join(' '),
      fill: 'none', stroke: spec.color, 'stroke-width': '1.6',
      'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    }));
    for (const p of points) {
      children.push(svgEl('circle', {
        cx: x(p.i), cy: y(p.s[spec.key]), r: '2', fill: spec.color,
      }, [svgEl('title', {}, [
        new Date(p.s.at).toLocaleTimeString() + ' · ' + spec.label + ': ' +
        p.s[spec.key].toFixed(1) + (unit === 'ms' ? ' ms' : ' ' + unit),
      ])]));
    }
  }
  if (samples.length) {
    children.push(svgEl('text', {
      x: left, y: height - 6, fill: '#8b93a3', 'font-size': '10',
    }, [new Date(samples[0].at).toLocaleTimeString()]));
    const last = samples[samples.length - 1];
    children.push(svgEl('text', {
      x: width - right, y: height - 6, 'text-anchor': 'end',
      fill: '#8b93a3', 'font-size': '10',
    }, [new Date(last.at).toLocaleTimeString()]));
  }
  return svgEl('svg', { viewBox: '0 0 ' + width + ' ' + height, class: 'chart-svg' }, children);
}

function chartBlock(title, samples, specs, unit) {
  const legend = el('div', { class: 'legend' }, specs.map((spec) =>
    el('span', { class: 'muted' }, [
      el('i', { class: 'swatch', style: 'background:' + spec.color }),
      spec.label,
    ])));
  return el('div', { class: 'chart' }, [el('h4', { text: title }), legend,
    lineChart(samples, specs, unit)]);
}

function connectionTable(m) {
  const conns = m.connections || [];
  if (!conns.length) return el('p', { class: 'muted', text: 'No per-connection samples yet.' });
  const rows = conns.map((c) => {
    const mode = pickPerfMode(c);
    return [
      c.connection,
      String(c.attempts),
      String(c.successes),
      fmtMs(mode.avg_ttft_ms),
      fmtMs(mode.avg_latency_ms),
      tpsCell(mode),
      perfModes(c),
      c.last_seen ? new Date(c.last_seen).toLocaleTimeString() : '—',
    ];
  });
  return table(
    ['connection', 'requests', 'ok', 'TTFT', 'latency', 'TPS', 'mode', 'last seen'],
    rows,
  );
}

// renderPerformance shows the last N attempts per advertised model. Stream and
// non-stream samples are averaged separately; the row prefers the stream values
// and labels a non-stream TPS as estimated (it is tokens/latency, not TPS).
// Clicking a row opens the time-series charts and the per-connection breakdown.
async function renderPerformance() {
  const host = el('div');
  const meta = el('span', { class: 'muted' });
  const expanded = new Set();
  let payload = null;
  let rows = [];

  function draw() {
    const tbody = el('tbody');
    for (const row of rows) {
      const key = row.provider + '/' + row.model;
      const open = expanded.has(key);
      const caret = el('span', { class: 'caret', text: open ? '▾' : '▸' });
      const tr = el('tr', { class: 'perf-row' });
      tr.appendChild(el('td', {}, [caret, document.createTextNode(' ' + key)]));
      for (const cell of row.cells.slice(1)) tr.appendChild(el('td', {}, [cell]));
      tr.addEventListener('click', () => {
        if (expanded.has(key)) expanded.delete(key);
        else expanded.add(key);
        draw();
      });
      tbody.appendChild(tr);
      if (open) {
        const details = el('div', { class: 'perf-detail' });
        details.append(
          el('div', { class: 'charts' }, [
            chartBlock('TTFT & latency (ms)', row.series, [
              { key: 'ttft_ms', label: 'TTFT', color: '#7ec699' },
              { key: 'latency_ms', label: 'latency', color: '#6ea8fe' },
            ], 'ms'),
            chartBlock('TPS (tokens/s)', row.series, [
              { key: 'tps', label: 'TPS', color: '#e0af68' },
            ], 'tokens/s'),
          ]),
          el('h4', { text: 'By connection' }),
          connectionTable(row.source),
        );
        tbody.appendChild(el('tr', { class: 'perf-detail-row' }, [
          el('td', { colspan: '9' }, [details]),
        ]));
      }
    }

    const head = el('thead', {}, [el('tr', {}, [
      'model', 'requests', 'ok', 'TTFT', 'latency', 'TPS', 'TPS trend',
      'mode', 'last seen',
    ].map((h) => el('th', { text: h })))]);
    host.replaceChildren(el('table', {}, [head, tbody]));
    if (payload && payload.data.every((m) => m.attempts === 0)) {
      host.appendChild(el('p', { class: 'muted', text: 'No samples yet — send a request to a model.' }));
    }
  }

  async function load() {
    try {
      payload = await api('/api/performance?series=1');
      rows = payload.data.map((m) => {
        const mode = pickPerfMode(m);
        return {
          provider: m.provider,
          model: m.model,
          source: m,
          series: m.series || [],
          tps: mode.avg_tps === null || mode.avg_tps === undefined ? -1 : mode.avg_tps,
          cells: [
            m.provider + '/' + m.model,
            String(m.attempts),
            String(m.successes),
            fmtMs(mode.avg_ttft_ms),
            fmtMs(mode.avg_latency_ms),
            tpsCell(mode),
            sparkline(m.series || [], 'tps'),
            perfModes(m),
            m.last_seen ? new Date(m.last_seen).toLocaleTimeString() : '—',
          ],
        };
      });
      rows.sort((a, b) => b.tps - a.tps ||
        (a.provider + '/' + a.model < b.provider + '/' + b.model ? -1
          : a.provider + '/' + a.model > b.provider + '/' + b.model ? 1 : 0));
      draw();
      meta.textContent = 'window: last ' + payload.window + ' attempts · low sample < ' +
        payload.low_sample_at + ' · updated ' + new Date().toLocaleTimeString();
    } catch (err) {
      host.replaceChildren(el('p', { class: 'error', text: err.message }));
      setStatus(err.message, 'error');
    }
  }

  await load();
  state.perfTimer = setInterval(() => {
    if (document.hidden || state.tab !== 'performance') return;
    load();
  }, 5000);

  view.replaceChildren(
    el('h2', { text: 'Performance' }),
    el('div', { class: 'toolbar' }, [
      meta,
      el('span', { class: 'muted', text: 'last N attempts per model; est. = tokens/latency; click a row for charts; refresh 5 s' }),
    ]),
    host,
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