'use strict';

// Gitrepo admin console. Talks to /api with the admin token kept in
// sessionStorage (cleared when the tab closes). All DOM is built with
// textContent and DOM nodes, never HTML strings, so API data cannot inject
// markup. The look comes from the StackEnigma design kit (stackenigma.css).

const TOKEN_KEY = 'gitrepo.token';
let token = sessionStorage.getItem(TOKEN_KEY) || '';
const app = document.getElementById('app');
const SVG_NS = 'http://www.w3.org/2000/svg';

// ------------------------------------------------------------------ helpers

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = Boolean(v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// fill replaces el's children, skipping null / false (replaceChildren would
// insert them as text).
function fill(el, ...nodes) {
  el.replaceChildren(...nodes.flat(Infinity).filter((n) => n !== null && n !== undefined && n !== false));
  return el;
}

// Small line icons (24×24 grid, stroked with the text colour).
const ICONS = {
  plus: ['M12 5v14', 'M5 12h14'],
  x: ['M6 6l12 12', 'M18 6 6 18'],
  refresh: ['M20 12a8 8 0 1 1-2.35-5.65', 'M20 4v4.5h-4.5'],
  copy: ['M9 9h11v11H9z', 'M5 15H4V4h11v1'],
  sync: ['M4 12a8 8 0 0 1 13.66-5.66L20 8.5', 'M20 4v4.5h-4.5', 'M20 12a8 8 0 0 1-13.66 5.66L4 15.5', 'M4 20v-4.5h4.5'],
  arrowLeft: ['M15 6l-6 6 6 6'],
  download: ['M12 4v11', 'M7 10l5 5 5-5', 'M5 20h14'],
  logo: [],
};
function icon(name, size) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  for (const [k, v] of Object.entries({ viewBox: '0 0 24 24', width: size || 16, height: size || 16, fill: 'none', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' })) {
    svg.setAttribute(k, String(v));
  }
  for (const d of ICONS[name] || []) {
    const p = document.createElementNS(SVG_NS, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}
// The brand mark, as in the sidebar.
function brandMark() {
  const mark = document.querySelector('.sidebar .brand-mark');
  return mark ? mark.cloneNode(true) : h('span', { class: 'brand-mark' });
}

// Top progress bar while any request is in flight.
let inFlight = 0;
function busy(delta) {
  inFlight += delta;
  document.getElementById('progress').hidden = inFlight <= 0;
}

async function api(method, path, body, opts) {
  const headers = { Authorization: 'Bearer ' + token };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  busy(1);
  try {
    const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    if (res.status === 401 || res.status === 403) {
      if (!(opts && opts.noLogout)) logout();
      throw new Error(res.status === 403 ? '该令牌没有 admin 权限' : '令牌无效，请重新登录');
    }
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (_) { /* not JSON */ }
    if (!res.ok) throw new Error((data && data.error) || text || res.statusText);
    return data;
  } finally {
    busy(-1);
  }
}

let toastTimer;
function toast(msg, bad) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast show' + (bad ? ' bad' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = 'toast' + (bad ? ' bad' : ''); }, bad ? 6000 : 2600);
}

function ago(iso) {
  if (!iso) return '从未';
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 0) return Math.round(-s) + ' 秒后';
  if (s < 60) return Math.round(s) + ' 秒前';
  if (s < 3600) return Math.round(s / 60) + ' 分钟前';
  if (s < 172800) return Math.round(s / 3600) + ' 小时前';
  return Math.round(s / 86400) + ' 天前';
}

function fmtDate(iso) { return iso ? new Date(iso).toLocaleString() : '—'; }
function fmtDay(iso) { return iso ? new Date(iso).toLocaleDateString() : '—'; }

const STATE_LABEL = {
  synced: '已同步', syncing: '同步中', pending: '等待中', behind: '落后',
  diverged: '分叉', failed: '失败', paused: '暂停', 'no-remotes': '无镜像',
  waiting: '等待标签', conflict: '冲突', draft: '草稿', unsupported: '不支持',
  deleted: '已删除', changed: '已修改', kept: '保持不变',
};
const STATE_TONE = {
  synced: 'ok', changed: 'ok', syncing: 'info', pending: 'neutral', waiting: 'warn', behind: 'warn',
  diverged: 'warn', failed: 'danger', conflict: 'danger', unsupported: 'warn',
};
function badge(state, text) {
  const tone = STATE_TONE[state] || 'neutral';
  return h('span', { class: 'badge' + (tone === 'neutral' ? '' : ' badge-' + tone) },
    tone === 'ok' || tone === 'danger' || tone === 'info' ? h('span', { class: 'dot' }) : null,
    text || STATE_LABEL[state] || state);
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('已复制');
  } catch (_) {
    // No clipboard API (plain http): select the text for a manual copy.
    const input = h('input', { class: 'input mono', readonly: true, value: text });
    openModal({ title: '复制', narrow: true, body: [h('p', { class: 'hint' }, '按 Ctrl+C（⌘C）复制：'), input] });
    input.select();
  }
}

// A read-only value with a copy button (clone URLs, commands).
function copyable(text, compact) {
  return h('div', { class: 'ro-field' + (compact ? ' ro-compact' : '') },
    h('span', { class: 'ro-value' }, text),
    h('button', { class: 'btn btn-sm btn-ghost btn-icon', type: 'button', title: '复制', 'aria-label': '复制', onclick: () => copyText(text) }, icon('copy', 15)));
}

function short(sha) { return sha ? sha.slice(0, 10) : '—'; }

// A table in a card, or the empty state. Columns headed '' hold actions.
function table(headers, rows, empty) {
  if (!rows.length) return h('div', { class: 'empty' }, empty || '暂无数据');
  return h('div', { class: 'card table-scroll' }, h('table', { class: 'table' },
    h('thead', {}, h('tr', {}, headers.map((x) => h('th', { class: x === '' ? 'actions' : null }, x)))),
    h('tbody', {}, rows)));
}

function field(label, input, cls, hint) {
  if (input instanceof Element && /^(INPUT|SELECT|TEXTAREA)$/.test(input.tagName) && input.type !== 'checkbox') input.classList.add('input');
  return h('label', { class: 'field' + (cls ? ' ' + cls : '') }, h('span', {}, label), input, hint ? h('small', { class: 'field-hint' }, hint) : null);
}
function check(name, label, checked, extra) {
  return h('label', { class: 'check' }, h('input', Object.assign({ type: 'checkbox', name, checked }, extra || {})), label);
}
function select(name, options, value) {
  return h('select', { name, class: 'input' }, options.map(([v, text]) => h('option', { value: v, selected: v === value }, text)));
}
function formData(form) {
  const out = {};
  for (const el of form.elements) {
    if (!el.name) continue;
    if (el.type === 'checkbox') {
      if (el.dataset.group) {
        (out[el.dataset.group] = out[el.dataset.group] || []);
        if (el.checked) out[el.dataset.group].push(el.value);
      } else out[el.name] = el.checked;
    } else if (el.type !== 'file') out[el.name] = el.value.trim();
  }
  return out;
}
function list(s) { return (s || '').split(',').map((x) => x.trim()).filter(Boolean); }

// Wraps a submit/click handler: shows the control as busy, reports errors.
function action(fn, okMsg) {
  return async (ev) => {
    if (ev && ev.preventDefault) ev.preventDefault();
    const ctl = ev && (ev.submitter || ev.currentTarget);
    const isButton = ctl && ctl.tagName === 'BUTTON';
    if (isButton) { ctl.disabled = true; ctl.classList.add('is-busy'); }
    try {
      const msg = await fn(ev);
      if (msg !== false) toast(typeof msg === 'string' ? msg : (okMsg || '完成'));
    } catch (e) {
      toast(e.message, true);
    } finally {
      if (isButton) { ctl.disabled = false; ctl.classList.remove('is-busy'); }
    }
  };
}

function enc(s) { return encodeURIComponent(s); }
function repoHash(slug) { return '#/repo/' + slug.split('/').map(enc).join('/'); }

// ----------------------------------------------------------------- layout

function pageHead(title, desc, actions, crumbs) {
  return h('div', { class: 'page-head' },
    h('div', { class: 'page-title' }, crumbs || null, h('h1', {}, title), desc ? h('p', { class: 'page-desc' }, desc) : null),
    actions && actions.length ? h('div', { class: 'rowflex page-actions' }, actions) : null);
}
function section(title, actions, ...children) {
  return h('section', { class: 'section' },
    h('div', { class: 'section-head' }, h('h2', {}, title), actions && actions.length ? h('div', { class: 'rowflex' }, actions) : null),
    children);
}
// toolbar: a hint on the left and actions on the right (inside a tab).
function toolbar(hint, actions) {
  return h('div', { class: 'toolbar' }, hint ? h('p', { class: 'hint' }, hint) : h('span'),
    actions && actions.length ? h('div', { class: 'rowflex' }, actions) : null);
}
function refreshButton() {
  return h('button', { class: 'btn btn-icon', type: 'button', title: '刷新', 'aria-label': '刷新', onclick: rerender }, icon('refresh'));
}
function addButton(label, onclick, primary) {
  return h('button', { class: 'btn' + (primary === false ? '' : ' btn-primary'), type: 'button', onclick }, icon('plus'), label);
}
function stat(label, value, tone) {
  return h('div', { class: 'card stat' + (tone ? ' stat-' + tone : '') }, h('div', { class: 'stat-label' }, label), h('div', { class: 'stat-value' }, String(value)));
}

// ----------------------------------------------------------------- dialogs

const modalRoot = document.getElementById('modal-root');
const openClosers = new Set();

// openModal shows a dialog. With onSubmit the body and footer are a form.
// It returns a function that closes the dialog.
function openModal({ title, body, foot, onSubmit, onClose, narrow, wide }) {
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    backdrop.remove();
    document.removeEventListener('keydown', onKey);
    openClosers.delete(close);
    if (onClose) onClose();
  };
  // Esc closes only the topmost dialog (a confirmation over a form).
  const onKey = (ev) => { if (ev.key === 'Escape' && modalRoot.lastElementChild === backdrop) close(); };
  const bodyEl = h('div', { class: 'modal-body' }, body);
  const footEl = foot ? h('div', { class: 'modal-foot' }, foot) : null;
  const inner = onSubmit ? h('form', { class: 'modal-form', onsubmit: onSubmit }, bodyEl, footEl) : [bodyEl, footEl];
  const backdrop = h('div', { class: 'modal-backdrop', onmousedown: (ev) => { if (ev.target === backdrop) close(); } },
    h('div', { class: 'modal' + (narrow ? ' modal-narrow' : '') + (wide ? ' modal-wide' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
      h('div', { class: 'modal-head' }, h('h2', {}, title),
        h('button', { class: 'btn btn-sm btn-ghost btn-icon', type: 'button', 'aria-label': '关闭', onclick: close }, icon('x'))),
      inner));
  modalRoot.append(backdrop);
  document.addEventListener('keydown', onKey);
  openClosers.add(close);
  const first = backdrop.querySelector('.modal-body input:not([type=hidden]):not([type=checkbox]):not([readonly]), .modal-body select, .modal-body textarea');
  if (first) first.focus();
  return close;
}

// formModal: a dialog around a form. submit(data, form) returns a toast
// message (or false for none); the dialog closes unless it returns KEEP_OPEN.
const KEEP_OPEN = Symbol('keep open');
function formModal({ title, intro, fields, submitText, danger, submit, wide }) {
  let close = null;
  close = openModal({
    title, wide,
    body: [intro ? h('p', { class: 'hint' }, intro) : null, h('div', { class: 'form-grid' }, fields)],
    foot: [
      h('button', { class: 'btn', type: 'button', onclick: () => close() }, '取消'),
      h('button', { class: 'btn ' + (danger ? 'btn-danger' : 'btn-primary'), type: 'submit' }, submitText || '保存')],
    onSubmit: action(async (ev) => {
      const res = await submit(formData(ev.target), ev.target);
      if (res === KEEP_OPEN) return false;
      close();
      return res;
    }),
  });
  return close;
}

// confirmDialog replaces window.confirm (and prompt-to-type confirmations).
function confirmDialog({ title, message, ok, danger, typeToConfirm }) {
  return new Promise((resolve) => {
    // The button that asked shows no spinner while the person decides.
    const paused = [...document.querySelectorAll('.is-busy')];
    for (const b of paused) b.classList.remove('is-busy');
    let settled = false;
    let close = null;
    const finish = (v) => {
      if (settled) return;
      settled = true;
      if (v) for (const b of paused) if (b.disabled) b.classList.add('is-busy');
      resolve(v);
      if (close) close();
    };
    const okBtn = h('button', { class: 'btn ' + (danger ? 'btn-danger' : 'btn-primary'), type: 'submit', disabled: Boolean(typeToConfirm) }, ok || '确定');
    let input = null;
    if (typeToConfirm) {
      input = h('input', { class: 'input mono', autocomplete: 'off', spellcheck: 'false', placeholder: typeToConfirm });
      input.addEventListener('input', () => { okBtn.disabled = input.value.trim() !== typeToConfirm; });
    }
    close = openModal({
      title: title || '确认', narrow: true,
      body: [String(message || '').split('\n').map((line) => h('p', {}, line)),
        typeToConfirm ? [h('p', { class: 'hint' }, '输入 ', h('code', {}, typeToConfirm), ' 确认：'), input] : null],
      foot: [h('button', { class: 'btn', type: 'button', onclick: () => finish(false) }, '取消'), okBtn],
      onSubmit: (ev) => { ev.preventDefault(); finish(true); },
      onClose: () => finish(false),
    });
    if (!input) okBtn.focus();
  });
}

// ------------------------------------------------------------------- router

const routes = [
  [/^#?\/?$/, viewRepos],
  [/^#\/repo\/([^/?]+)\/([^/?]+)(?:\?tab=([a-z]+))?$/, viewRepo],
  [/^#\/targets$/, viewTargets],
  [/^#\/bindings$/, viewBindings],
  [/^#\/protect$/, viewProtect],
  [/^#\/tokens$/, viewTokens],
  [/^#\/webhooks$/, viewWebhooks],
  [/^#\/audit(?:\?.*)?$/, viewAudit],
];

async function render() {
  for (const close of [...openClosers]) close();
  document.body.classList.toggle('logged-out', !token);
  if (!token) return viewLogin();
  const hash = location.hash || '#/';
  for (const a of document.querySelectorAll('#nav .nav-link')) {
    const target = a.getAttribute('href');
    const on = target === '#/' ? (hash === '#/' || hash.startsWith('#/repo/')) : hash.startsWith(target);
    a.classList.toggle('active', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  }
  for (const [re, view] of routes) {
    const m = hash.match(re);
    if (!m) continue;
    try {
      await view(...m.slice(1).map((x) => (x === undefined ? undefined : decodeURIComponent(x))));
    } catch (e) {
      fill(app, pageHead('出错了'), h('div', { class: 'card card-pad error-text' }, e.message));
    }
    return;
  }
  fill(app, pageHead('页面不存在'), h('div', { class: 'empty' }, h('a', { href: '#/' }, '回到仓库列表')));
}

function rerender() { return render(); }

function logout() {
  token = '';
  sessionStorage.removeItem(TOKEN_KEY);
  render();
}

function setupTheme() {
  const sw = document.getElementById('theme-switch');
  const mark = () => {
    const c = window.gitrepoTheme ? window.gitrepoTheme.get() : 'system';
    for (const b of sw.querySelectorAll('.tab')) b.classList.toggle('active', b.dataset.themeChoice === c);
  };
  sw.addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-theme-choice]');
    if (!b || !window.gitrepoTheme) return;
    window.gitrepoTheme.set(b.dataset.themeChoice);
    mark();
  });
  mark();
}

document.getElementById('logout').addEventListener('click', logout);
window.addEventListener('hashchange', render);
setupTheme();
render();

// -------------------------------------------------------------------- login

function viewLogin() {
  const input = h('input', { type: 'password', name: 'token', autocomplete: 'current-password', placeholder: 'GITREPO_ADMIN_TOKEN 或 admin 令牌', required: true });
  const err = h('p', { class: 'error-text', hidden: true });
  const form = h('form', { class: 'card login-card', onsubmit: action(async (ev) => {
    token = input.value.trim();
    try {
      await api('GET', '/api/status', undefined, { noLogout: true });
    } catch (e) {
      token = '';
      err.textContent = e.message;
      err.hidden = false;
      return false;
    }
    sessionStorage.setItem(TOKEN_KEY, token);
    render();
    return false;
  }) },
  h('h1', {}, '登录'),
  h('p', { class: 'hint' }, '使用部署时设置的管理令牌（GITREPO_ADMIN_TOKEN），或具有 admin 权限的令牌。令牌只保存在当前标签页。'),
  field('管理令牌', input), err,
  h('button', { class: 'btn btn-primary', type: 'submit' }, '登录'));
  fill(app, h('div', { class: 'login' },
    h('div', { class: 'login-brand' }, brandMark(), 'Gitrepo'),
    form,
    h('p', { class: 'faint login-foot' }, '一个地址 push，同步到 GitHub、GitLab、Gitea、Gitee 和任意 Git 服务器。')));
  input.focus();
}

// -------------------------------------------------------------------- repos

const SLUG_PATTERN = '[A-Za-z0-9][A-Za-z0-9._\\-]*/[A-Za-z0-9][A-Za-z0-9._\\-]*';
const VISIBILITY_LABEL = { public: '公开', internal: '内部', private: '私有' };

function targetOptions(targets) {
  return targets.map((t) => [t.name, `${t.name}（${t.provider}${t.namespace ? ' · ' + t.namespace : ''}）`]);
}

async function viewRepos() {
  const [repos, targets, bindings] = await Promise.all([api('GET', '/api/status'), api('GET', '/api/targets'), api('GET', '/api/bindings')]);
  const rows = repos.map((r) => {
    const synced = r.remotes.filter((m) => m.state === 'synced').length;
    let oldest = null;
    let never = false;
    for (const m of r.remotes) {
      if (!m.last_success_at) never = true;
      else if (!oldest || m.last_success_at < oldest) oldest = m.last_success_at;
    }
    return h('tr', {},
      h('td', {}, h('a', { class: 'cell-title', href: repoHash(r.slug) }, r.slug),
        r.visibility === 'public' ? h('div', { class: 'cell-sub' }, '公开') : null),
      h('td', {}, h('div', { class: 'state-cell' }, badge(r.state), r.busy ? h('span', { class: 'faint' }, '同步中…') : null)),
      h('td', { class: 'nowrap' }, r.remotes.length ? `${synced} / ${r.remotes.length}` : '—'),
      h('td', { class: 'nowrap' }, r.remotes.length ? (never ? '从未' : ago(oldest)) : '—'),
      h('td', {}, copyable(r.clone_url, true)));
  });
  const attention = repos.filter((r) => ['failed', 'diverged', 'behind'].includes(r.state)).length;
  const mirrors = repos.reduce((n, r) => n + r.remotes.length, 0);
  fill(app, 
    pageHead('仓库', '每个仓库一个 Hub 地址：push 到这里，同步到它的所有镜像。', [
      h('button', { class: 'btn', type: 'button', onclick: () => importModal(targets) }, '从镜像导入'),
      addButton('新建仓库', () => newRepoModal()),
      refreshButton()]),
    !targets.length || !bindings.length ? quickStart(targets, bindings) : null,
    repos.length ? h('div', { class: 'kpi-grid stats' },
      stat('仓库', repos.length), stat('全部同步', repos.filter((r) => r.state === 'synced').length, 'ok'),
      stat('需要处理', attention, attention ? 'danger' : null), stat('镜像', mirrors)) : null,
    table(['仓库', '状态', '已同步镜像', '上次全部同步', 'Push / Clone 地址'], rows,
      [h('div', { class: 'empty-title' }, '还没有仓库'),
        h('div', {}, '用有 create 权限的令牌直接 push 到 ', h('code', {}, location.origin + '/<组织>/<仓库>.git'), '，仓库会自动创建。')]));
}

function newRepoModal() {
  formModal({
    title: '新建仓库', submitText: '创建',
    intro: '也可以不在这里创建：用有 create 权限的令牌直接 push 到新地址，仓库会自动创建。',
    fields: [
      field('仓库（组织/名称）', h('input', { name: 'slug', placeholder: 'acme/demo', required: true, pattern: SLUG_PATTERN }), 'wide'),
      field('可见性', select('visibility', [['private', '私有'], ['public', '公开（允许匿名读取，镜像也设为公开）']], 'private'), 'wide'),
      h('div', { class: 'checks' }, check('bindings', '应用备份规则（按组织自动添加镜像）', true))],
    submit: async (d) => {
      const r = await api('POST', '/api/repos', { slug: d.slug, visibility: d.visibility, bindings: d.bindings });
      location.hash = repoHash(r.slug);
      return '已创建 ' + r.slug;
    },
  });
}

function importModal(targets) {
  if (!targets.length) {
    toast('请先添加平台凭据', true);
    location.hash = '#/targets';
    return;
  }
  formModal({
    title: '从已有镜像导入', submitText: '导入',
    intro: '把 GitHub、GitLab 等平台上已有的仓库（包括 LFS 内容）作为 Hub 的初始内容，然后同步到其他镜像。Hub 数据丢失时，也用它从任一镜像恢复。',
    fields: [
      field('Hub 仓库（组织/名称）', h('input', { name: 'slug', placeholder: 'acme/demo', required: true, pattern: SLUG_PATTERN })),
      field('从哪个平台导入', select('target', targetOptions(targets))),
      field('命名空间（可选）', h('input', { name: 'namespace', placeholder: '默认使用凭据上的命名空间' })),
      field('平台上的仓库名（可选）', h('input', { name: 'name', placeholder: '默认与 Hub 同名' })),
      h('div', { class: 'checks' }, check('force', '替换 Hub 上已有的内容（先备份到 refs/backup/）'))],
    submit: async (d) => {
      if (d.force && !(await confirmDialog({ title: '替换 Hub 上的内容', message: 'Hub 上已有的分支和标签会被替换（先备份到 refs/backup/）。继续？', ok: '替换', danger: true }))) return KEEP_OPEN;
      const res = await api('POST', '/api/repos/' + d.slug.split('/').map(enc).join('/') + '/import', { target: d.target, namespace: d.namespace, name: d.name, force: d.force });
      location.hash = repoHash(res.repo.slug);
      const warn = (res.import.warnings || []).length ? '（有警告，见同步记录）' : '';
      return `已从 ${res.source} 导入 ${res.import.refs} 个 ref、${res.import.lfs_objects} 个 LFS 对象${warn}`;
    },
  });
}

function quickStart(targets, bindings) {
  const step = (done, ...text) => h('li', { class: done ? 'done' : null }, h('span', { class: 'step-text' }, text));
  return h('div', { class: 'card card-pad quickstart' },
    h('h2', {}, '开始使用'),
    h('ol', { class: 'steps' },
      step(targets.length, h('a', { href: '#/targets' }, '填入平台凭据'), '：GitHub、GitLab、Gitea/Forgejo、Gitee 或任意 Git 服务器。'),
      step(bindings.length, h('a', { href: '#/bindings' }, '添加备份规则'), '：例如所有组织（*）的新仓库都备份到 GitHub 和 GitLab。'),
      step(false, h('a', { href: '#/tokens' }, '创建访问令牌'), '（create 权限），作为 git 的 HTTPS 密码。'),
      step(false, 'push：', h('code', {}, `git push ${location.origin}/<组织>/<仓库>.git main`))));
}

const REPO_TABS = [['mirrors', '镜像'], ['releases', 'Release'], ['backups', '备份'], ['runs', '同步记录'], ['audit', '操作记录'], ['settings', '设置']];

async function viewRepo(org, name, tab) {
  const slug = org + '/' + name;
  const base = '/api/repos/' + enc(org) + '/' + enc(name);
  const [repo, runs, targets, backups, audit, releases] = await Promise.all([api('GET', base), api('GET', base + '/runs?limit=30'), api('GET', '/api/targets'), api('GET', base + '/backups'),
    api('GET', '/api/audit?limit=10&repo=' + enc(slug)), api('GET', base + '/releases')]);

  const syncNow = (overwrite) => action(async () => {
    if (overwrite && !(await confirmDialog({ title: '以 Hub 为准覆盖分叉', message: '以 Hub 为准覆盖所有分叉的镜像 ref？\n被覆盖的镜像内容会先备份到 Hub 的 refs/backup/ 下。', ok: '覆盖', danger: true }))) return false;
    const res = await api('POST', base + '/sync', { wait: true, overwrite });
    rerender();
    const bad = (res.events || []).filter((e) => e.type === 'remote' && e.state !== 'synced' && e.state !== 'paused');
    return bad.length ? `完成，${bad.length} 个镜像未同步` : '已同步';
  });

  // ---- mirrors
  const remoteRows = repo.remotes.map((m) => {
    const path = base + '/remotes/' + m.id;
    const notes = [];
    if (m.needs_adoption) {
      notes.push(h('div', { class: 'notice notice-warn' },
        '镜像仓库里已有 Gitrepo 没写过的内容。确认是正确的仓库后再接管。',
        h('div', {}, h('button', { class: 'btn btn-sm btn-primary', type: 'button', onclick: action(async () => {
          if (!(await confirmDialog({ title: '接管镜像', message: `确认 ${m.url} 是 ${slug} 的镜像，交给 Gitrepo 管理？\n已有分支只会快进或报告分叉，不会被覆盖。`, ok: '接管' }))) return false;
          await api('PATCH', path, { adopt: true });
          rerender();
        }, '已接管，正在同步') }, '接管'))));
    }
    if (m.last_error) notes.push(h('div', { class: 'notice notice-danger' }, `${m.last_error}（连续失败 ${m.consecutive_failures || 0} 次${m.next_retry_at ? '，下次重试 ' + ago(m.next_retry_at) : ''}）`));
    if (m.divergences && m.divergences.length) {
      notes.push(h('div', { class: 'notice notice-warn' },
        h('ul', { class: 'div-list' }, m.divergences.map((d) => h('li', {},
          h('code', {}, d.ref), ' ', d.reason, h('span', { class: 'faint' }, ` 镜像 ${short(d.remote_sha)} · Hub ${short(d.hub_sha)} · 上次推送 ${short(d.expected_sha)}`)))),
        h('div', {}, '解决：把镜像的提交合并后 push 到 Hub，或使用上方的“以 Hub 为准覆盖分叉”。')));
    }
    return h('tr', {},
      h('td', {}, h('div', { class: 'cell-title' }, m.label), h('div', { class: 'cell-sub mono' }, m.url),
        m.view ? h('div', { class: 'cell-sub' }, h('span', { class: 'badge badge-info' }, '部分同步'), ' ', viewLabel(m)) : null,
        notes.length ? h('div', { class: 'mirror-notes' }, notes) : null),
      h('td', {}, badge(m.enabled ? m.state : 'paused')),
      h('td', { class: 'nowrap' }, ago(m.last_success_at)),
      h('td', { class: 'nowrap' }, m.on_divergence === 'overwrite' ? '备份后覆盖' : '告警'),
      h('td', { class: 'actions' },
        h('button', { class: 'btn btn-sm', type: 'button', title: '同步哪些内容', onclick: () => viewModal(path, m) }, '视图'),
        h('button', { class: 'btn btn-sm', type: 'button', onclick: action(async () => { await api('PATCH', path, { enabled: !m.enabled }); rerender(); }, m.enabled ? '已暂停' : '已恢复') }, m.enabled ? '暂停' : '恢复'),
        h('button', { class: 'btn btn-sm', type: 'button', title: '切换分叉策略', onclick: action(async () => {
          const next = m.on_divergence === 'overwrite' ? 'alert' : 'overwrite';
          if (next === 'overwrite' && !(await confirmDialog({ title: '分叉策略', message: `分叉时自动以 Hub 为准覆盖 ${m.label}（会先备份）？`, ok: '改为覆盖' }))) return false;
          await api('PATCH', path, { on_divergence: next });
          rerender();
        }, '已更新') }, m.on_divergence === 'overwrite' ? '改为告警' : '改为覆盖'),
        h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
          if (!(await confirmDialog({ title: '移除镜像', message: `停止备份到 ${m.label}？\n远端仓库不会被删除。`, ok: '移除', danger: true }))) return false;
          await api('DELETE', path);
          rerender();
        }, '已移除') }, '移除')));
  });
  const addMirror = () => {
    if (!targets.length) { location.hash = '#/targets'; return; }
    formModal({
      title: '添加镜像', submitText: '添加镜像',
      fields: [
        field('平台凭据', select('target', targetOptions(targets)), 'wide'),
        field('命名空间（可选）', h('input', { name: 'namespace', placeholder: '默认使用凭据上的命名空间' })),
        field('远端仓库名（可选）', h('input', { name: 'name', placeholder: name })),
        field('完整 URL（可选，覆盖推导）', h('input', { name: 'url', placeholder: 'ssh://git@git.example.com/x/y.git' }), 'wide'),
        field('分叉时', select('on_divergence', [['alert', '告警，不覆盖'], ['overwrite', '备份后覆盖']], 'alert')),
        viewFields(null, true)],
      submit: async (d) => {
        await api('POST', base + '/remotes', Object.assign(viewBody(d), {
          target: d.target, namespace: d.namespace, name: d.name, url: d.url, on_divergence: d.on_divergence }));
        rerender();
        return '已添加，正在首次同步';
      },
    });
  };

  // ---- runs
  const runRows = runs.map((r) => {
    const dur = r.finished_at && r.started_at ? ((new Date(r.finished_at) - new Date(r.started_at)) / 1000).toFixed(1) + 's' : '—';
    return h('tr', {}, h('td', { class: 'nowrap', title: fmtDate(r.started_at) }, ago(r.started_at)), h('td', {}, r.remote), h('td', {}, r.trigger), h('td', {}, badge(r.status)),
      h('td', {}, r.refs_changed), h('td', { class: 'nowrap' }, dur), h('td', { class: 'error-text' }, r.error || ''));
  });

  // ---- backups
  const backupRows = backups.map((b) => h('tr', {},
    h('td', { class: 'nowrap', title: fmtDate(b.created_at) }, ago(b.created_at)),
    h('td', {}, b.remote || (b.source === 'hub' ? 'Hub' : b.source)),
    h('td', { class: 'mono' }, b.original),
    h('td', { class: 'mono' }, short(b.sha)),
    h('td', {}, b.subject || ''),
    h('td', { class: 'nowrap' }, fmtDay(b.expires_at)),
    h('td', { class: 'actions' },
      h('button', { class: 'btn btn-sm', type: 'button', onclick: action(async () => {
        const res = await api('POST', base + '/backups/restore', { backup: b.ref });
        rerender();
        return `已恢复为分支 ${res.branch}`;
      }) }, '恢复为分支'),
      h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
        if (!(await confirmDialog({ title: '彻底清除备份', message: '这个备份在 Hub 上的内容会被删除，无法恢复。', ok: '清除', danger: true, typeToConfirm: slug }))) return false;
        await api('POST', base + '/backups/purge', { backups: [b.ref], lfs: true });
        rerender();
      }, '已清除') }, '清除'))));

  // ---- info card
  const toggleVisibility = action(async () => {
    const next = repo.visibility === 'public' ? 'private' : 'public';
    const label = VISIBILITY_LABEL[next];
    if (!(await confirmDialog({ title: '修改可见性', message: `把 ${slug} 改为${label}？\n跟随仓库可见性的镜像也会通过平台 API 一起修改。`, ok: '改为' + label }))) return false;
    const res = await api('PATCH', base, { visibility: next });
    rerender();
    const bad = res.mirrors.filter((m) => m.state === 'failed' || m.state === 'unsupported');
    if (bad.length) throw new Error(`已改为${label}，但这些镜像没有改：` + bad.map((m) => `${m.mirror}（${m.detail}）`).join('；'));
    return `已改为${label}` + (res.mirrors.length ? `，${res.mirrors.filter((m) => m.state === 'changed').length} 个镜像已同步修改` : '');
  });
  const info = h('div', { class: 'card card-pad' }, h('dl', { class: 'kv' },
    h('dt', {}, 'Push / Clone'), h('dd', {}, copyable(repo.clone_url)),
    repo.ssh_url ? [h('dt', {}, 'SSH'), h('dd', {}, copyable(repo.ssh_url))] : null,
    h('dt', {}, '可见性'), h('dd', {}, h('div', { class: 'rowflex' }, h('span', { class: 'badge' + (repo.visibility === 'public' ? ' badge-info' : '') }, VISIBILITY_LABEL[repo.visibility] || repo.visibility),
      h('button', { class: 'btn btn-sm', type: 'button', onclick: toggleVisibility }, repo.visibility === 'public' ? '改为私有' : '改为公开'))),
    h('dt', {}, '默认分支'), h('dd', {}, repo.default_branch ? h('code', {}, repo.default_branch) : h('span', { class: 'muted' }, '（空仓库）')),
    (repo.protected || []).length ? [h('dt', {}, '受保护'), h('dd', {}, h('div', { class: 'rowflex' },
      repo.protected.map((p) => h('span', { class: 'badge' }, p.ref, ' · ', protectLabel(p))), h('a', { href: '#/protect' }, '管理')))] : null,
    h('dt', {}, '添加远端'), h('dd', {}, copyable(`git remote add hub ${repo.clone_url}`))));

  // ---- tabs
  const panels = {
    mirrors: () => [toolbar('push 到 Hub 后按顺序同步到这些镜像；失败的会自动重试。', [addButton('添加镜像', addMirror, false)]),
      table(['镜像', '状态', '上次成功', '分叉时', ''], remoteRows,
        [h('div', { class: 'empty-title' }, '还没有镜像'), h('div', {}, targets.length ? '添加一个镜像，或在“备份规则”里为这个组织配置默认镜像。' : '请先添加平台凭据。'),
          targets.length ? addButton('添加镜像', addMirror) : h('a', { class: 'btn btn-primary', href: '#/targets' }, '添加平台凭据')])],
    releases: () => [toolbar('在这里发布一次，说明和附件会发布到所有 GitHub、GitLab、Gitea/Forgejo、Gitee 镜像；标签同步到某个镜像后才会在那里创建。平台上已有的、不是 Gitrepo 建的同名 Release 不会被覆盖（显示“冲突”）。',
      [addButton('新建 Release', () => releaseModal(base), false)]),
      releaseTable(base, releases)],
    backups: () => [toolbar('被强推覆盖、删除或导入替换的历史会在 Hub 上隐藏保留一段时间（默认 30 天），clone 看不到。泄露了密钥时，请轮换密钥并清除备份。'),
      table(['时间', '来源', '原 ref', '提交', '说明', '到期', ''], backupRows, '没有备份。')],
    runs: () => [table(['时间', '镜像', '触发', '结果', '变更', '耗时', '错误'], runRows, '暂无记录。')],
    audit: () => [toolbar('最近 10 条。', [h('a', { class: 'btn btn-sm', href: '#/audit?repo=' + enc(slug) }, '查看全部')]), auditTable(audit.entries, false)],
    settings: () => [h('div', { class: 'card card-pad danger-zone' }, h('div', { class: 'rowflex' },
      h('div', {}, h('h3', {}, '注销仓库'), h('p', { class: 'hint' }, 'Hub 上的数据移入 trash 目录，各镜像不受影响。')),
      h('span', { class: 'spacer' }),
      h('button', { class: 'btn btn-danger', type: 'button', onclick: action(async () => {
        if (!(await confirmDialog({ title: '注销仓库', message: `注销 ${slug}？Hub 上的数据会移入 trash 目录。`, ok: '注销', danger: true, typeToConfirm: slug }))) return false;
        await api('DELETE', base);
        location.hash = '#/';
      }, '已注销') }, '注销仓库')))],
  };
  const counts = { mirrors: repo.remotes.length, releases: releases.length, backups: backups.length };
  let current = panels[tab] ? tab : 'mirrors';
  const panel = h('div', { class: 'tab-panel' });
  const bar = h('div', { class: 'tabnav-bar', role: 'tablist' });
  const show = (key) => {
    current = key;
    for (const b of bar.children) {
      b.classList.toggle('active', b.dataset.tab === key);
      b.setAttribute('aria-selected', String(b.dataset.tab === key));
    }
    fill(panel, panels[key]());
    history.replaceState(null, '', repoHash(slug) + (key === 'mirrors' ? '' : '?tab=' + key));
  };
  for (const [key, label] of REPO_TABS) {
    bar.append(h('button', { class: 'tab', type: 'button', role: 'tab', 'data-tab': key, onclick: () => show(key) },
      label, counts[key] ? h('span', { class: 'count' }, String(counts[key])) : null));
  }

  fill(app, 
    pageHead([slug, badge(repo.state)], null, [
      h('button', { class: 'btn btn-primary', type: 'button', onclick: syncNow(false) }, icon('sync'), '立即同步'),
      h('button', { class: 'btn', type: 'button', onclick: syncNow(true) }, '以 Hub 为准覆盖分叉'),
      refreshButton()],
    h('div', { class: 'crumbs' }, h('a', { href: '#/' }, '仓库'), h('span', { class: 'sep' }, '/'), h('span', {}, org))),
    info,
    h('div', { class: 'section' }, h('div', { class: 'tabnav' }, bar), panel));
  show(current);
}

// ------------------------------------------------------------ mirror views

// A mirror's view: only part of the repository (a directory split, some
// branches, renamed tags). Branch and tag maps are edited one per line.
function splitMaps(refs) {
  const branches = [];
  const tags = [];
  const other = [];
  for (const r of refs || []) {
    const [src, dst] = r.split(':');
    if (src.startsWith('refs/heads/') && dst.startsWith('refs/heads/')) {
      const a = src.slice(11);
      const b = dst.slice(11);
      branches.push(a === b ? a : a + ':' + b);
    } else if (src.startsWith('refs/tags/') && dst.startsWith('refs/tags/')) {
      const a = src.slice(10);
      const b = dst.slice(10);
      tags.push(a === b ? a : a + ':' + b);
    } else other.push(r);
  }
  return { branches, tags, other };
}

function viewLabel(m) {
  if (!m.subdir && !(m.refs || []).length) return '全部分支和标签';
  const maps = splitMaps(m.refs);
  const arrow = (s) => s.replace(':', ' → ');
  const parts = [];
  if (m.subdir) parts.push('目录 ' + m.subdir + '/');
  parts.push(maps.branches.length ? '分支 ' + maps.branches.map(arrow).join('、') : '全部分支');
  if (maps.tags.length) parts.push('标签 ' + maps.tags.map(arrow).join('、'));
  else if (!m.subdir && !maps.branches.length && !maps.other.length) parts.push('全部标签');
  else if (!maps.other.length) parts.push('不同步标签');
  if (maps.other.length) parts.push(maps.other.map(arrow).join('、'));
  return parts.join(' · ');
}

function viewFields(m, collapsible) {
  const maps = splitMaps(m ? m.refs : []);
  const fields = [
    field('只同步这个目录（可选）', h('input', { name: 'subdir', value: m ? m.subdir || '' : '', placeholder: 'services/api' }), 'wide',
      '把这个目录的历史拆出来，作为镜像仓库的根目录（monorepo 拆分）。拆出的提交是新的，在镜像上的修改无法回流。'),
    field('只同步这些分支（可选，每行一个）', h('textarea', { name: 'branches', rows: 2, placeholder: 'api:main' }), null,
      '源:目标，例如 api:main 把 Hub 的 api 分支推成镜像的 main；可以用 *。留空时同步全部分支'),
    field('同步这些标签（可选，每行一个）', h('textarea', { name: 'tags', rows: 2, placeholder: 'api-v*:v*' }), null,
      '例如 api-v*:v* 把 api-v1.0 推成 v1.0。留空时：完整镜像同步全部标签；拆目录或只选了部分分支时不同步标签'),
  ];
  fields[1].querySelector('textarea').value = maps.branches.join('\n');
  fields[2].querySelector('textarea').value = maps.tags.join('\n');
  if (maps.other.length) fields.push(h('input', { type: 'hidden', name: 'other_refs', value: maps.other.join('\n') }));
  if (!collapsible) return fields;
  const open = Boolean(m && (m.subdir || (m.refs || []).length));
  return h('details', { class: 'wide view-fields', open },
    h('summary', {}, '只同步部分内容（可选）：一个目录、几个分支或改名的标签'), h('div', { class: 'form-grid' }, fields));
}

function viewBody(d) {
  const lines = (s) => (s || '').split('\n').map((x) => x.trim()).filter(Boolean);
  const refs = lines(d.branches).concat(lines(d.tags).map((t) => {
    const [src, dst] = t.split(':');
    return 'refs/tags/' + src + ':refs/tags/' + (dst || src);
  }), lines(d.other_refs));
  return { refs, subdir: d.subdir || '' };
}

function viewModal(path, m) {
  formModal({
    title: '镜像视图：' + m.label, submitText: '保存', wide: true,
    intro: '决定这个镜像同步 Hub 的哪些内容。保存后的下一次同步会按新视图改写镜像：不再包含的分支和标签会在镜像上删除，改变的会被覆盖（原来的值备份在 Hub 的 refs/backup/ 下）。',
    fields: viewFields(m, false),
    submit: async (d) => {
      await api('PATCH', path, viewBody(d));
      rerender();
      return '已保存，正在同步';
    },
  });
}

// ------------------------------------------------------------------ targets

const PROVIDERS = [
  ['github', 'GitHub / GitHub Enterprise'],
  ['gitlab', 'GitLab（gitlab.com 或自建）'],
  ['gitea', 'Gitea / Forgejo'],
  ['gitee', 'Gitee（码云）'],
  ['generic', '其他 Git 服务器（无 API）'],
];
const PROVIDER_LABEL = { github: 'GitHub', gitlab: 'GitLab', gitea: 'Gitea', gitee: 'Gitee', generic: 'Git' };
const CRED_LABEL = { token: '访问令牌', basic: '用户名 + 密码', ssh: 'SSH 私钥', none: '匿名', 'github-app': 'GitHub App' };

async function viewTargets() {
  const targets = await api('GET', '/api/targets');
  const rows = targets.map((t) => h('tr', {},
    h('td', {}, h('div', { class: 'cell-title' }, t.name), h('div', { class: 'cell-sub' }, PROVIDER_LABEL[t.provider] || t.provider)),
    h('td', {}, t.namespace || h('span', { class: 'faint' }, '—')),
    h('td', { class: 'mono' }, t.url_template || t.base_url || '（默认）'),
    h('td', {}, t.credential_kind === 'github-app' ? `GitHub App ${t.app_id}` : (CRED_LABEL[t.credential_kind] || t.credential_kind),
      t.credential_kind === 'github-app' ? h('div', { class: 'cell-sub' }, '安装 ' + t.installation_id) : null,
      t.credential_username ? h('div', { class: 'cell-sub' }, t.credential_username) : null),
    h('td', { class: 'nowrap' }, t.expires_at ? fmtDay(t.expires_at) : h('span', { class: 'faint' }, '—'),
      t.expires_detected ? h('div', { class: 'cell-sub' }, '从平台读取') : null),
    h('td', { class: 'actions' },
      t.provider !== 'generic' ? h('button', { class: 'btn btn-sm', type: 'button', onclick: action(async () => {
        const res = await api('POST', '/api/targets/' + enc(t.name) + '/check');
        if (!res.ok) throw new Error('验证失败：' + res.error);
        return '验证通过：' + res.identity;
      }) }, '验证') : null,
      t.provider !== 'generic' ? h('button', { class: 'btn btn-sm', type: 'button', onclick: () => writeCheck(t) }, '写入检查') : null,
      h('button', { class: 'btn btn-sm', type: 'button', onclick: () => editTargetModal(t) }, '修改'),
      h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
        if (!(await confirmDialog({ title: '删除平台凭据', message: `删除平台凭据 ${t.name}？`, ok: '删除', danger: true }))) return false;
        await api('DELETE', '/api/targets/' + enc(t.name));
        rerender();
      }, '已删除') }, '删除'))));

  fill(app, 
    pageHead('平台凭据', '每个平台账号填一次。凭据用主密钥加密后入库，界面和 API 都不会再显示。', [addButton('添加平台凭据', targetModal), refreshButton()]),
    table(['名称', '命名空间', '地址', '凭据', '过期', ''], rows,
      [h('div', { class: 'empty-title' }, '还没有平台凭据'), h('div', {}, '添加 GitHub、GitLab、Gitea、Gitee 的账号，或任意 Git 服务器。'), addButton('添加平台凭据', targetModal)]));
}

async function writeCheck(t) {
  const ok = await confirmDialog({
    title: '写入检查：' + t.name,
    message: `在 ${PROVIDER_LABEL[t.provider] || t.provider} 上建一个临时私有仓库 gitrepo-check-…，把同步和发布要用到的写操作（建仓、LFS、推送、默认分支、可见性、Release 和附件）都试一遍，最后删除它。`,
    ok: '开始检查',
  });
  if (!ok) return;
  const body = h('div', {}, h('div', { class: 'spinner-row' }, h('span', { class: 'spinner' }), '正在平台上检查，可能需要一分钟…'));
  const done = h('button', { class: 'btn', type: 'button', disabled: true }, '关闭');
  const close = openModal({ title: '写入检查：' + t.name, body, foot: [done], wide: true });
  done.addEventListener('click', () => close());
  try {
    const res = await api('POST', '/api/targets/' + enc(t.name) + '/check', { write: true });
    if (res.error) throw new Error('验证失败：' + res.error);
    const label = { ok: '通过', failed: '失败', skipped: '跳过' };
    const tone = { ok: 'synced', failed: 'failed', skipped: 'paused' };
    fill(body, 
      h('div', { class: 'check-steps' }, (res.steps || []).map((st) => h('div', { class: 'check-step' },
        badge(tone[st.state] || st.state, label[st.state] || st.state),
        h('div', { class: 'step-name' }, st.name),
        h('div', { class: 'step-detail' }, st.detail || '')))),
      res.leftover ? h('p', { class: 'error-text' }, `临时仓库 ${res.leftover} 没能删除，请到平台上手动删除。`) : null);
    toast(res.ok ? '写入检查通过' : '写入检查没有通过', !res.ok);
  } catch (e) {
    fill(body, h('p', { class: 'error-text' }, e.message));
  } finally {
    done.disabled = false;
  }
}

const PROVIDER_HELP = {
  github: 'Fine-grained token：Contents 读写；自动建仓还需 Administration 读写。也可以用 GitHub App：不绑定个人，令牌每小时自动换新。命名空间填拥有镜像的用户或组织。',
  gitlab: 'Access token：api + write_repository。命名空间填用户或 group 路径（可含子 group）。自建实例填写站点地址。',
  gitea: 'Access token：repository 读写；在组织下建仓需 organization 写权限。站点地址必填。',
  gitee: '私人令牌：勾选 projects 权限。命名空间填个人空间或组织路径；git 用户名会自动从令牌查出。',
  generic: '没有 API，仓库需事先存在。URL 模板中 {namespace} 和 {name} 会被替换。',
};

function targetModal() {
  const provider = select('provider', PROVIDERS, 'github');
  const KINDS = [['token', '访问令牌'], ['github-app', 'GitHub App（推荐给组织）'], ['basic', '用户名 + 密码'], ['ssh', 'SSH 私钥'], ['none', '无（匿名）']];
  const kind = select('kind', KINDS, 'token');
  const help = h('p', { class: 'hint' });
  const dyn = h('div', { class: 'form-grid wide' });

  function update() {
    const p = provider.value;
    help.textContent = PROVIDER_HELP[p];
    const nodes = [];
    nodes.push(field('命名空间' + (p === 'generic' ? '（可选）' : ''), h('input', { name: 'namespace', required: p !== 'generic', placeholder: p === 'gitlab' ? 'acme-group' : 'acme' })));
    if (p === 'generic') {
      nodes.push(field('URL 模板', h('input', { name: 'url_template', required: true, placeholder: 'https://git.example.com/{namespace}/{name}.git' }), 'wide'));
      kind.disabled = false;
    } else {
      const site = { github: 'https://github.com', gitlab: 'https://gitlab.com', gitee: 'https://gitee.com', gitea: 'https://git.example.com' };
      nodes.push(field('站点地址' + (p === 'gitea' ? '' : '（自建时填写）'), h('input', { name: 'base_url', required: p === 'gitea', placeholder: site[p] })));
      if (p !== 'github' || (kind.value !== 'token' && kind.value !== 'github-app')) kind.value = 'token';
      kind.disabled = p !== 'github';
    }
    for (const o of kind.options) o.hidden = p === 'generic' ? o.value === 'github-app' : (p === 'github' ? !['token', 'github-app'].includes(o.value) : o.value !== 'token');
    const k = kind.value;
    if (k === 'github-app') {
      const keyArea = h('textarea', { name: 'secret', required: true, placeholder: '-----BEGIN RSA PRIVATE KEY-----' });
      nodes.push(field('App ID', h('input', { name: 'app_id', required: true, inputmode: 'numeric', pattern: '[0-9]+', placeholder: '123456' })));
      nodes.push(field('安装 ID（可选）', h('input', { name: 'installation_id', inputmode: 'numeric', pattern: '[0-9]*' }), null, '默认按命名空间查找'));
      nodes.push(field('私钥（App 设置页生成的 .pem）', keyArea, 'wide'));
      nodes.push(field('或选择 .pem 文件', h('input', { type: 'file', accept: '.pem', onchange: async (ev) => {
        const f = ev.target.files[0];
        if (f) keyArea.value = await f.text();
      } }), 'wide'));
      nodes.push(h('p', { class: 'hint' }, 'App 需要的仓库权限：Contents 读写、Metadata 只读；自动建仓还需 Administration 读写；推送 .github/workflows 需 Workflows 读写。安装时选 All repositories，新建的镜像才在 App 的范围内。只能在组织下自动建仓。'));
      fill(dyn, nodes);
      return;
    }
    if (k === 'basic' || (k === 'token' && p === 'generic')) nodes.push(field('用户名' + (k === 'token' ? '（可选）' : ''), h('input', { name: 'username', required: k === 'basic' })));
    if (k === 'ssh') {
      nodes.push(field('SSH 私钥', h('textarea', { name: 'secret', required: true, placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----' }), 'wide'));
      nodes.push(field('known_hosts（可选）', h('textarea', { name: 'ssh_known_hosts' }), 'wide', '填写后严格校验主机密钥'));
    } else if (k !== 'none') {
      nodes.push(field(k === 'basic' ? '密码' : '访问令牌', h('input', { name: 'secret', type: 'password', required: true, autocomplete: 'off' })));
    }
    nodes.push(field('凭据过期日（可选）', h('input', { name: 'expires_at', type: 'date' }), null, 'GitHub、GitLab 令牌会自动读取'));
    fill(dyn, nodes);
  }
  provider.addEventListener('change', update);
  kind.addEventListener('change', update);
  update();

  formModal({
    title: '添加平台凭据', submitText: '添加', wide: true,
    fields: [
      field('名称', h('input', { name: 'name', required: true, placeholder: 'github', pattern: '[A-Za-z0-9._\\-]+' })),
      field('平台', provider),
      h('div', { class: 'wide' }, help),
      field('凭据类型', kind, 'wide'),
      dyn,
      field('镜像可见性', select('visibility', [['', '跟随仓库'], ['private', '始终私有'], ['public', '始终公开']], '')),
      h('div', { class: 'checks' }, check('auto_create', '自动创建缺失的远端仓库', true))],
    submit: async (d) => {
      const body = {
        name: d.name, provider: provider.value, namespace: d.namespace || '',
        base_url: d.base_url || '', url_template: d.url_template || '',
        auto_create: d.auto_create, visibility: d.visibility,
        credential: kind.value === 'github-app'
          ? { kind: 'github-app', app_id: d.app_id, installation_id: d.installation_id || '', secret: d.secret }
          : { kind: kind.value, username: d.username || '', secret: d.secret || '', ssh_known_hosts: d.ssh_known_hosts || '' },
      };
      if (d.expires_at) body.expires_at = d.expires_at;
      await api('POST', '/api/targets', body);
      rerender();
      return '已添加 ' + d.name;
    },
  });
}

function editTargetModal(t) {
  const isKey = t.credential_kind === 'ssh' || t.credential_kind === 'github-app';
  formModal({
    title: '修改 ' + t.name, wide: true,
    fields: [
      field('命名空间', h('input', { name: 'namespace', value: t.namespace || '' })),
      t.provider === 'generic'
        ? field('URL 模板', h('input', { name: 'url_template', value: t.url_template || '' }), 'wide')
        : field('站点地址', h('input', { name: 'base_url', value: t.base_url || '' })),
      field('单文件上限 MiB', h('input', { name: 'max_file_size', type: 'number', min: '0', value: t.max_file_size ? Math.round(t.max_file_size / 1048576) : '' }), null, '0 或留空 = 平台默认'),
      field('LFS 对象上限 MiB', h('input', { name: 'max_lfs_object_size', type: 'number', min: '0', value: t.max_lfs_object_size ? Math.round(t.max_lfs_object_size / 1048576) : '' }), null, '0 或留空 = 平台默认'),
      field('凭据过期日', h('input', { name: 'expires_at', type: 'date', value: t.expires_at ? t.expires_at.slice(0, 10) : '' }), null,
        t.expires_detected ? '当前日期从平台读取' : '清空则从平台读取'),
      t.credential_kind === 'none' ? null : field(t.credential_kind === 'github-app' ? '新 App 私钥（留空不变）' : (isKey ? '新 SSH 私钥（留空不变）' : '新令牌 / 密码（留空不变）'),
        isKey ? h('textarea', { name: 'secret' }) : h('input', { name: 'secret', type: 'password', autocomplete: 'off' }), isKey ? 'wide' : null)],
    submit: async (d) => {
      const body = { namespace: d.namespace };
      // Only a changed date is sent: an untouched detected date stays detected.
      if ((d.expires_at || '') !== (t.expires_at ? t.expires_at.slice(0, 10) : '')) body.expires_at = d.expires_at || '';
      if (t.provider === 'generic') body.url_template = d.url_template;
      else body.base_url = d.base_url;
      if (d.max_file_size !== '') body.max_file_size = Number(d.max_file_size) * 1024 * 1024;
      if (d.max_lfs_object_size !== '') body.max_lfs_object_size = Number(d.max_lfs_object_size) * 1024 * 1024;
      // Username omitted: the server keeps it, or looks it up again (Gitee).
      if (d.secret) body.credential = { kind: t.credential_kind, secret: d.secret };
      await api('PATCH', '/api/targets/' + enc(t.name), body);
      rerender();
      return '已保存';
    },
  });
}

// ----------------------------------------------------------------- bindings

async function viewBindings() {
  const [bindings, targets] = await Promise.all([api('GET', '/api/bindings'), api('GET', '/api/targets')]);
  const rows = bindings.map((b) => h('tr', {},
    h('td', {}, b.org === '*' ? h('span', {}, '所有组织 ', h('code', {}, '*')) : h('span', { class: 'cell-title' }, b.org)),
    h('td', {}, b.target),
    h('td', {}, b.namespace || h('span', { class: 'faint' }, '凭据默认')),
    h('td', { class: 'mono' }, b.name_template),
    h('td', {}, b.on_divergence === 'overwrite' ? '备份后覆盖' : '告警'),
    h('td', { class: 'actions' }, h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
      if (!(await confirmDialog({ title: '删除备份规则', message: '删除这条规则？已有仓库的镜像不受影响。', ok: '删除', danger: true }))) return false;
      await api('DELETE', '/api/bindings/' + b.id);
      rerender();
    }, '已删除') }, '删除'))));
  const add = () => {
    if (!targets.length) { location.hash = '#/targets'; return; }
    formModal({
      title: '添加备份规则', submitText: '添加规则',
      intro: '多个组织备份到同一个命名空间时，把镜像仓库名设为 {org}-{name} 以免重名；同一个镜像位置只能属于一个 Hub 仓库。',
      fields: [
        field('组织', h('input', { name: 'org', value: '*', required: true }), null, '* 表示全部组织'),
        field('平台凭据', select('target', targets.map((t) => [t.name, `${t.name}（${t.provider}）`]))),
        field('命名空间（可选）', h('input', { name: 'namespace', placeholder: '默认使用凭据上的命名空间' })),
        field('镜像仓库名', h('input', { name: 'name_template', value: '{name}', required: true }), null, '{org} 和 {name} 是 Hub 仓库的组织和名称'),
        field('分叉时', select('on_divergence', [['alert', '告警，不覆盖'], ['overwrite', '备份后覆盖']], 'alert')),
        h('div', { class: 'checks' }, check('apply_existing', '同时应用到已有仓库'))],
      submit: async (d) => {
        const res = await api('POST', '/api/bindings', d);
        rerender();
        if (res.skipped && res.skipped.length) throw new Error(`已添加；${res.skipped.length} 个仓库因镜像名冲突被跳过：\n` + res.skipped.join('\n'));
        return d.apply_existing ? `已添加，并应用到 ${res.applied_to_repos} 个已有仓库` : '已添加';
      },
    });
  };
  fill(app, 
    pageHead('备份规则', '新仓库创建时（包括 push 自动创建），按规则自动添加镜像。', [addButton('添加规则', add)]),
    table(['组织', '平台凭据', '命名空间', '镜像仓库名', '分叉时', ''], rows,
      [h('div', { class: 'empty-title' }, '还没有规则'),
        h('div', {}, targets.length ? '例如：所有组织（*）的新仓库都备份到 GitHub。' : '请先添加平台凭据。'),
        targets.length ? addButton('添加规则', add) : h('a', { class: 'btn btn-primary', href: '#/targets' }, '添加平台凭据')]));
}

// ------------------------------------------------------------------ protect

function protectLabel(p) {
  return [p.deny_force ? '禁止强推' : '', p.deny_delete ? '禁止删除' : ''].filter(Boolean).join('、');
}

async function viewProtect() {
  const rules = await api('GET', '/api/protections');
  const rows = rules.map((p) => h('tr', {},
    h('td', {}, p.repos === '*' ? h('span', {}, '所有仓库 ', h('code', {}, '*')) : h('code', {}, p.repos)),
    h('td', {}, h('code', {}, p.ref)),
    h('td', {}, h('div', { class: 'rowflex' }, p.deny_force ? h('span', { class: 'badge badge-info' }, '禁止强推') : null, p.deny_delete ? h('span', { class: 'badge badge-info' }, '禁止删除') : null)),
    h('td', { class: 'actions' }, h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
      if (!(await confirmDialog({ title: '取消保护', message: `取消对 ${p.repos} 中 ${p.ref} 的保护？`, ok: '取消保护', danger: true }))) return false;
      await api('DELETE', '/api/protections/' + p.id);
      rerender();
    }, '已删除') }, '删除'))));
  const add = () => formModal({
    title: '添加保护规则', submitText: '添加',
    fields: [
      field('仓库', h('input', { name: 'repos', value: '*', required: true }), null, '* 表示全部，也可以是 acme/* 或 acme/app'),
      field('分支', h('input', { name: 'ref', value: 'main', required: true }), null, '如 main、release/*；标签写 refs/tags/v*'),
      h('div', { class: 'checks' }, check('deny_force', '禁止强推（改写历史）', true), check('deny_delete', '禁止删除', true))],
    submit: async (d) => {
      await api('POST', '/api/protections', { repos: d.repos, ref: d.ref, deny_force: d.deny_force, deny_delete: d.deny_delete });
      rerender();
      return '已添加';
    },
  });
  fill(app, 
    pageHead('分支保护', '受保护的分支在 Hub 上不能被强推或删除（HTTPS 和 SSH 都一样），镜像也就不会被改写；受保护的标签不能被移动或删除。', [addButton('添加规则', add)]),
    table(['仓库', '分支 / 标签', '规则', ''], rows,
      [h('div', { class: 'empty-title' }, '还没有规则'), h('div', {}, '例如保护所有仓库的 main：仓库 * ，分支 main。'), addButton('添加规则', add)]),
    h('p', { class: 'hint section' }, '* 不跨越 /。确实需要改写时（例如清除误提交的密钥），先删除对应规则，完成后再加回来；这些操作都记在审计日志里。'));
}

// ------------------------------------------------------------------- tokens

async function viewTokens() {
  const [tokens, keys, sshInfo] = await Promise.all([api('GET', '/api/tokens'), api('GET', '/api/ssh-keys'), api('GET', '/api/ssh')]);
  const scopeBadges = (scopes) => h('div', { class: 'rowflex' }, scopes.map((s) => h('span', { class: 'badge' + (s === 'admin' ? ' badge-warn' : '') }, s)));
  const rows = tokens.map((t) => h('tr', {},
    h('td', { class: 'cell-title' }, t.name), h('td', {}, scopeBadges(t.scopes)), h('td', {}, (t.repos || []).join(', ') || h('span', { class: 'faint' }, '全部')),
    h('td', { class: 'nowrap' }, ago(t.last_used_at)),
    h('td', { class: 'actions' }, h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
      if (!(await confirmDialog({ title: '吊销令牌', message: `吊销令牌 ${t.name}？使用它的客户端会立即失去访问。`, ok: '吊销', danger: true }))) return false;
      await api('DELETE', '/api/tokens/' + t.id);
      rerender();
    }, '已吊销') }, '吊销'))));
  const createToken = () => formModal({
    title: '创建令牌', submitText: '创建令牌',
    fields: [
      field('名称', h('input', { name: 'name', required: true, placeholder: 'laptop', pattern: '[A-Za-z0-9._\\-]+' })),
      field('权限', select('scope', [['read', 'read：clone / fetch'], ['write', 'write：另外可以 push'], ['create', 'create：另外可以 push 新地址建仓'], ['admin', 'admin：另外可以管理']], 'create')),
      field('仓库范围（可选）', h('input', { name: 'repos', placeholder: 'acme/*, tools/cli' }), 'wide', '默认全部仓库')],
    submit: async (d) => {
      const t = await api('POST', '/api/tokens', { name: d.name, scopes: [d.scope], repos: list(d.repos) });
      await rerender();
      let close = null;
      close = openModal({
        title: `令牌 ${t.name} 已创建`,
        body: [h('p', {}, '只显示这一次，请现在保存：'), h('div', { class: 'secret' }, t.token),
          h('div', { class: 'rowflex' }, h('button', { class: 'btn btn-sm', type: 'button', onclick: () => copyText(t.token) }, icon('copy', 15), '复制令牌')),
          h('p', { class: 'hint' }, '作为 git 的 HTTPS 密码使用（用户名任意），例如：'),
          h('code', {}, `git push https://x:${t.token.slice(0, 8)}…@${location.host}/acme/demo.git main`)],
        foot: [h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close() }, '我已保存')],
      });
      return false;
    },
  });

  const keyRows = keys.map((k) => h('tr', {},
    h('td', { class: 'cell-title' }, k.name), h('td', {}, k.type), h('td', { class: 'mono' }, k.fingerprint),
    h('td', {}, scopeBadges(k.scopes)), h('td', {}, (k.repos || []).join(', ') || h('span', { class: 'faint' }, '全部')), h('td', { class: 'nowrap' }, ago(k.last_used_at)),
    h('td', { class: 'actions' }, h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
      if (!(await confirmDialog({ title: '删除 SSH 公钥', message: `删除 SSH 公钥 ${k.name}？`, ok: '删除', danger: true }))) return false;
      await api('DELETE', '/api/ssh-keys/' + k.id);
      rerender();
    }, '已删除') }, '删除'))));
  const addKey = () => formModal({
    title: '添加 SSH 公钥', submitText: '添加公钥', wide: true,
    fields: [
      field('名称', h('input', { name: 'name', required: true, placeholder: 'laptop', pattern: '[A-Za-z0-9._\\-]+' })),
      field('权限', select('scope', [['read', 'read'], ['write', 'write'], ['create', 'create']], 'create')),
      field('仓库范围（可选）', h('input', { name: 'repos', placeholder: 'acme/*' }), 'wide', '默认全部仓库'),
      field('公钥', h('textarea', { name: 'public_key', required: true, placeholder: 'ssh-ed25519 AAAA… you@laptop' }), 'wide', '~/.ssh/id_ed25519.pub 的内容')],
    submit: async (d) => {
      await api('POST', '/api/ssh-keys', { name: d.name, public_key: d.public_key, scopes: [d.scope], repos: list(d.repos) });
      rerender();
      return '已添加';
    },
  });
  const sshSection = sshInfo.enabled
    ? section('SSH 公钥', [addButton('添加公钥', addKey, false)],
      h('div', { class: 'card card-pad' }, h('dl', { class: 'kv' },
        h('dt', {}, '地址示例'), h('dd', {}, copyable(sshInfo.example_url)),
        h('dt', {}, '主机指纹'), h('dd', {}, h('code', {}, sshInfo.fingerprint), h('span', { class: 'faint' }, '  首次连接时请核对')))),
      table(['名称', '类型', '指纹', '权限', '仓库范围', '上次使用', ''], keyRows, '还没有公钥。'))
    : section('SSH 公钥', null, h('div', { class: 'empty' }, 'SSH 未启用。在服务端设置 ssh.listen（或环境变量 GITREPO_SSH_LISTEN，例如 :2222）后即可通过 SSH push 和 clone。'));

  fill(app, 
    pageHead('访问令牌', '给开发者、CI 和部署平台使用。只保存哈希，丢失后只能重新创建。', [addButton('创建令牌', createToken)]),
    table(['名称', '权限', '仓库范围', '上次使用', ''], rows,
      [h('div', { class: 'empty-title' }, '还没有令牌'), h('div', {}, '创建一个 create 权限的令牌，作为 git 的 HTTPS 密码即可 push。'), addButton('创建令牌', createToken)]),
    sshSection);
}

// ----------------------------------------------------------------- webhooks

const EVENTS = [
  ['push', 'push：收到推送'],
  ['sync.completed', 'sync.completed：镜像有更新'],
  ['remote.diverged', 'remote.diverged：镜像分叉'],
  ['remote.failed', 'remote.failed：连续失败'],
  ['remote.recovered', 'remote.recovered：恢复'],
  ['credential.expiring', 'credential.expiring：凭据将过期'],
];
const FORMAT_LABEL = { json: 'JSON', wecom: '企业微信', feishu: '飞书', dingtalk: '钉钉', slack: 'Slack' };

async function viewWebhooks() {
  const hooks = await api('GET', '/api/webhooks');
  const rows = hooks.map((w) => h('tr', {},
    h('td', {}, h('div', { class: 'cell-title' }, w.name), h('div', { class: 'cell-sub' }, (FORMAT_LABEL[w.format] || w.format) + (w.signed ? ' · 已签名' : ''))),
    h('td', { class: 'mono' }, w.host),
    h('td', {}, w.events.join(', ')), h('td', {}, (w.repos || []).join(', ') || h('span', { class: 'faint' }, '全部')),
    h('td', {}, w.enabled ? badge('synced', '启用') : badge('paused', '停用')),
    h('td', { class: 'nowrap' }, w.last_delivery_at ? [String(w.last_status), h('div', { class: 'cell-sub' }, ago(w.last_delivery_at))] : h('span', { class: 'faint' }, '从未')),
    h('td', { class: 'actions' },
      h('button', { class: 'btn btn-sm', type: 'button', onclick: action(async () => {
        const res = await api('POST', `/api/webhooks/${w.id}/test`);
        rerender();
        if (!res.ok) throw new Error('投递失败：' + res.error);
        return '测试投递成功';
      }) }, '测试'),
      h('button', { class: 'btn btn-sm', type: 'button', onclick: action(async () => { await api('PATCH', `/api/webhooks/${w.id}`, { enabled: !w.enabled }); rerender(); }) }, w.enabled ? '停用' : '启用'),
      h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
        if (!(await confirmDialog({ title: '删除通知', message: `删除通知 ${w.name}？`, ok: '删除', danger: true }))) return false;
        await api('DELETE', `/api/webhooks/${w.id}`);
        rerender();
      }, '已删除') }, '删除'))));
  const add = () => formModal({
    title: '添加通知', submitText: '添加通知', wide: true,
    fields: [
      field('名称', h('input', { name: 'name', required: true, placeholder: 'oncall', pattern: '[A-Za-z0-9._\\-]+' })),
      field('格式', select('format', [['json', 'JSON（带 HMAC 签名，给程序用）'], ['wecom', '企业微信机器人'], ['feishu', '飞书机器人'], ['dingtalk', '钉钉机器人'], ['slack', 'Slack']], 'json')),
      field('URL', h('input', { name: 'url', type: 'url', required: true, placeholder: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…' }), 'wide', '加密保存（机器人地址里带着 key）'),
      h('div', { class: 'wide' }, h('span', { class: 'checks-label' }, '事件'),
        h('div', { class: 'checks' }, EVENTS.map(([v, text]) => check('ev_' + v, text, v !== 'push', { value: v, 'data-group': 'events' })))),
      field('仓库范围（可选）', h('input', { name: 'repos', placeholder: 'acme/*' }), null, '默认全部；限定范围后收不到 credential.expiring'),
      field('签名密钥（JSON 格式可选）', h('input', { name: 'secret', type: 'password', autocomplete: 'off' }))],
    submit: async (d) => {
      const events = d.events && d.events.length === EVENTS.length ? ['*'] : (d.events || []);
      if (!events.length) throw new Error('至少选择一个事件');
      await api('POST', '/api/webhooks', { name: d.name, url: d.url, format: d.format, events, repos: list(d.repos), secret: d.secret });
      rerender();
      return '已添加，可以点“测试”验证';
    },
  });

  fill(app, 
    pageHead('通知', '分叉、失败等告警只在状态变化时发送一次；sync.completed 只在镜像确实有更新时发送，载荷包含 ref 和 SHA，可用于触发下游构建。', [addButton('添加通知', add)]),
    table(['名称', '目标', '事件', '仓库', '状态', '最近投递', ''], rows,
      [h('div', { class: 'empty-title' }, '还没有通知'), h('div', {}, '支持签名 webhook，以及企业微信、飞书、钉钉、Slack 机器人。'), addButton('添加通知', add)]));
}

// -------------------------------------------------------------------- audit

const VIA_LABEL = { api: 'API', http: 'HTTPS', ssh: 'SSH' };

function auditRow(e, showRepo) {
  let what;
  if (e.path) {
    what = h('code', {}, e.action.split(' ')[0] + ' ' + e.path);
  } else {
    what = [h('code', {}, e.action), showRepo && e.repo ? h('div', { class: 'cell-sub' }, h('a', { href: repoHash(e.repo) }, e.repo)) : null];
  }
  let result;
  if (!e.status) result = badge('synced', '已接受');
  else if (e.status < 400) result = badge('synced', String(e.status));
  else result = badge('failed', String(e.status));
  return h('tr', {},
    h('td', { class: 'nowrap', title: fmtDate(e.at) }, fmtDate(e.at)),
    h('td', { class: 'nowrap' }, h('div', { class: 'cell-title' }, e.actor), h('div', { class: 'cell-sub' }, (VIA_LABEL[e.via] || e.via) + (e.client_addr ? ' · ' + e.client_addr : ''))),
    h('td', {}, what),
    h('td', {}, result),
    h('td', {}, e.detail || ''));
}

function auditTable(entries, showRepo) {
  return table(['时间', '操作者', '操作', '结果', '说明'], entries.map((e) => auditRow(e, showRepo)), '暂无记录。');
}

async function viewAudit() {
  const params = new URLSearchParams((location.hash.split('?')[1]) || '');
  const filters = { actor: params.get('actor') || '', repo: params.get('repo') || '', action: params.get('action') || '' };
  const query = (extra) => {
    const q = new URLSearchParams({ limit: '100' });
    for (const [k, v] of Object.entries(filters)) if (v) q.set(k, v);
    for (const [k, v] of Object.entries(extra || {})) q.set(k, v);
    return '/api/audit?' + q.toString();
  };
  const page = await api('GET', query());
  const wrap = auditTable(page.entries, true);
  const tbody = wrap.querySelector('tbody');
  let next = page.next || 0;
  const more = h('button', { class: 'btn', type: 'button', hidden: !next, onclick: action(async () => {
    const p = await api('GET', query({ before: String(next) }));
    for (const e of p.entries) tbody.append(auditRow(e, true));
    next = p.next || 0;
    more.hidden = !next;
    return false;
  }) }, '加载更早的记录');

  const filtered = filters.actor || filters.repo || filters.action;
  const form = h('form', { class: 'card card-pad filter-bar', onsubmit: (ev) => {
    ev.preventDefault();
    const d = formData(ev.target);
    const q = new URLSearchParams();
    for (const k of ['actor', 'repo', 'action']) if (d[k]) q.set(k, d[k]);
    const target = '#/audit' + (q.toString() ? '?' + q.toString() : '');
    if (location.hash === target) render(); else location.hash = target;
  } },
  field('操作者', h('input', { name: 'actor', value: filters.actor, placeholder: 'admin、令牌名、ssh:公钥名' })),
  field('仓库', h('input', { name: 'repo', value: filters.repo, placeholder: 'acme/app' })),
  field('操作包含', h('input', { name: 'action', value: filters.action, placeholder: 'push、DELETE、targets' })),
  h('div', { class: 'rowflex' }, h('button', { class: 'btn btn-primary', type: 'submit' }, '筛选'),
    filtered ? h('a', { class: 'btn btn-ghost', href: '#/audit' }, '清除') : null));

  fill(app, 
    pageHead('审计日志', '记录所有修改类 API 调用（包括被拒绝的）和 Hub 接受的每次 push：谁、何时、从哪个地址、做了什么。请求内容（凭据等）从不记录。默认保留 365 天。', [refreshButton()]),
    form,
    wrap,
    h('div', { class: 'table-foot' }, more));
}

// ----------------------------------------------------------------- releases

function releaseTable(base, releases) {
  const path = (r) => base + '/releases/' + enc(r.tag);
  return table(['标签', '标题', '附件', '镜像', '创建', ''], releases.map((r) => h('tr', {},
    h('td', {}, h('code', {}, r.tag), r.published ? null : h('div', { class: 'cell-sub' }, badge('draft'))),
    h('td', {}, r.name, r.prerelease ? h('div', { class: 'cell-sub' }, '预发布') : null),
    h('td', {}, r.assets.length ? r.assets.map((a) => h('div', {}, h('a', { href: a.url, download: a.name, onclick: action(async (ev) => {
      // The API needs the token, so fetch the file instead of following the link.
      ev.preventDefault();
      const res = await fetch(a.url.replace(/^https?:\/\/[^/]+/, ''), { headers: { Authorization: 'Bearer ' + token } });
      if (!res.ok) throw new Error('下载失败：' + res.status);
      const blob = await res.blob();
      const link = h('a', { href: URL.createObjectURL(blob), download: a.name });
      link.click();
      setTimeout(() => URL.revokeObjectURL(link.href), 10000);
      return false;
    }) }, a.name), h('span', { class: 'faint' }, ' ' + fmtSize(a.size)))) : h('span', { class: 'faint' }, '—')),
    h('td', {}, r.mirrors.length ? h('div', { class: 'mirror-states' }, r.mirrors.map((m) => h('div', { title: m.error || '' }, badge(m.state), ' ', h('span', { class: 'faint' }, m.mirror)))) : h('span', { class: 'faint' }, '—')),
    h('td', { class: 'nowrap' }, ago(r.created_at)),
    h('td', { class: 'actions' },
      h('button', { class: 'btn btn-sm', type: 'button', onclick: action(async () => {
        const adopt = r.mirrors.some((m) => m.state === 'conflict') && await confirmDialog({
          title: '接管已有的 Release', message: '有镜像上已存在同名 Release（不是 Gitrepo 建的）。要接管并覆盖它的标题和说明吗？它原有的附件会保留。', ok: '接管' });
        const res = await api('POST', path(r) + '/publish', { wait: true, adopt });
        rerender();
        const bad = res.mirrors.filter((m) => m.state === 'failed' || m.state === 'conflict');
        return bad.length ? `${bad.length} 个镜像未发布，失败的会自动重试` : (res.mirrors.some((m) => m.state === 'waiting') ? '已发布；部分镜像等标签同步后再创建' : '已发布到所有镜像');
      }) }, r.published ? '重新发布' : '发布'),
      h('button', { class: 'btn btn-sm btn-danger-soft', type: 'button', onclick: action(async () => {
        if (!(await confirmDialog({ title: '删除 Release', message: `删除 Release ${r.tag}？\n各镜像上由 Gitrepo 创建的 Release 也会被删除（标签保留）。`, ok: '删除', danger: true }))) return false;
        await api('DELETE', path(r));
        rerender();
      }, '已删除') }, '删除')))),
  [h('div', { class: 'empty-title' }, '还没有 Release'), h('div', {}, '把标签推到 Hub 后，在这里发布一次，就会发布到所有镜像。'), addButton('新建 Release', () => releaseModal(base))]);
}

function fmtSize(n) {
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KiB';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MiB';
  return (n / 1073741824).toFixed(2) + ' GiB';
}

function releaseModal(base) {
  const files = h('input', { type: 'file', name: 'files', multiple: true });
  formModal({
    title: '新建 Release', submitText: '创建', wide: true,
    fields: [
      field('标签', h('input', { name: 'tag', required: true, placeholder: 'v1.2.0' }), null, '需要已推到 Hub'),
      field('标题', h('input', { name: 'name', placeholder: '默认与标签相同' })),
      field('说明', h('textarea', { name: 'body', class: 'prose', placeholder: '更新内容…' }), 'wide'),
      field('附件', files, 'wide'),
      h('div', { class: 'checks' }, check('prerelease', '预发布'), check('draft', '只保存为草稿，暂不发布'))],
    submit: async (d) => {
      if (!d.tag) throw new Error('请填写标签');
      await api('POST', base + '/releases', { tag: d.tag, name: d.name || d.tag, body: d.body, prerelease: d.prerelease, publish: false });
      const path = base + '/releases/' + enc(d.tag);
      for (const f of files.files) {
        toast(`正在上传 ${f.name}…`);
        busy(1);
        let res;
        try {
          res = await fetch(path + '/assets/' + enc(f.name), {
            method: 'PUT', body: f, headers: { Authorization: 'Bearer ' + token, 'Content-Type': f.type || 'application/octet-stream' },
          });
        } finally {
          busy(-1);
        }
        if (!res.ok) {
          let msg = res.statusText;
          try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
          throw new Error(`${f.name}：${msg}`);
        }
      }
      if (!d.draft) await api('POST', path + '/publish', { wait: true });
      rerender();
      return d.draft ? '已保存为草稿' : '已发布';
    },
  });
}
