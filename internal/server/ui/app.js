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