'use strict';

// Gitrepo admin console. Talks to /api with the admin token kept in
// sessionStorage (cleared when the tab closes). All DOM is built with
// textContent and DOM nodes, never HTML strings, so API data cannot inject
// markup.

const TOKEN_KEY = 'gitrepo.token';
let token = sessionStorage.getItem(TOKEN_KEY) || '';
const app = document.getElementById('app');

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

async function api(method, path, body, opts) {
  const headers = { Authorization: 'Bearer ' + token };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
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
}

let toastTimer;
function toast(msg, bad) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'show' + (bad ? ' bad' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = ''; }, bad ? 6000 : 2500);
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
};
function badge(state) { return h('span', { class: 'badge ' + state }, STATE_LABEL[state] || state); }

function copyable(text) {
  const btn = h('button', { class: 'secondary small', type: 'button', onclick: async () => {
    try { await navigator.clipboard.writeText(text); toast('已复制'); }
    catch (_) { window.prompt('复制：', text); }
  } }, '复制');
  return h('span', { class: 'copy' }, h('code', {}, text), btn);
}

function short(sha) { return sha ? sha.slice(0, 10) : '—'; }

function table(headers, rows, emptyText) {
  if (!rows.length) return h('div', { class: 'table-wrap' }, h('div', { class: 'empty' }, emptyText || '暂无数据'));
  return h('div', { class: 'table-wrap' }, h('table', {},
    h('thead', {}, h('tr', {}, headers.map((x) => h('th', {}, x)))),
    h('tbody', {}, rows)));
}

function field(label, input, cls) { return h('label', { class: cls }, label, input); }
function select(name, options, value) {
  return h('select', { name }, options.map(([v, text]) => h('option', { value: v, selected: v === value }, text)));
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
    } else out[el.name] = el.value.trim();
  }
  return out;
}
function list(s) { return (s || '').split(',').map((x) => x.trim()).filter(Boolean); }

// Wraps a submit/click handler: disables the control, reports errors.
function action(fn, okMsg) {
  return async (ev) => {
    if (ev && ev.preventDefault) ev.preventDefault();
    const ctl = ev && (ev.submitter || ev.currentTarget);
    if (ctl && 'disabled' in ctl) ctl.disabled = true;
    try {
      const msg = await fn(ev);
      if (msg !== false) toast(typeof msg === 'string' ? msg : (okMsg || '完成'));
    } catch (e) {
      toast(e.message, true);
    } finally {
      if (ctl && 'disabled' in ctl) ctl.disabled = false;
    }
  };
}

function enc(s) { return encodeURIComponent(s); }

// ------------------------------------------------------------------- router

const routes = [
  [/^#?\/?$/, viewRepos],
  [/^#\/repo\/([^/]+)\/([^/]+)$/, viewRepo],
  [/^#\/targets$/, viewTargets],
  [/^#\/bindings$/, viewBindings],
  [/^#\/protect$/, viewProtect],
  [/^#\/tokens$/, viewTokens],
  [/^#\/webhooks$/, viewWebhooks],
  [/^#\/audit(?:\?.*)?$/, viewAudit],
];

async function render() {
  const nav = document.getElementById('nav');
  document.getElementById('logout').hidden = !token;
  nav.hidden = !token;
  if (!token) return viewLogin();
  const hash = location.hash || '#/';
  for (const a of nav.querySelectorAll('a')) {
    const target = a.getAttribute('href');
    a.classList.toggle('active', target === '#/' ? (hash === '#/' || hash.startsWith('#/repo/')) : hash.startsWith(target));
  }
  for (const [re, view] of routes) {
    const m = hash.match(re);
    if (!m) continue;
    try {
      await view(...m.slice(1).map(decodeURIComponent));
    } catch (e) {
      app.replaceChildren(h('div', { class: 'panel error-text' }, e.message));
    }
    return;
  }
  app.replaceChildren(h('p', {}, '页面不存在。'));
}

function rerender() { render(); }

function logout() {
  token = '';
  sessionStorage.removeItem(TOKEN_KEY);
  render();
}

document.getElementById('logout').addEventListener('click', logout);
window.addEventListener('hashchange', render);
render();

// -------------------------------------------------------------------- login

function viewLogin() {
  const input = h('input', { type: 'password', name: 'token', autocomplete: 'current-password', placeholder: 'GITREPO_ADMIN_TOKEN 或 admin 令牌', required: true });
  const err = h('p', { class: 'error-text' });
  const form = h('form', { class: 'panel', onsubmit: async (ev) => {
    ev.preventDefault();
    token = input.value.trim();
    try {
      await api('GET', '/api/status', undefined, { noLogout: true });
      sessionStorage.setItem(TOKEN_KEY, token);
      render();
    } catch (e) {
      token = '';
      err.textContent = e.message;
    }
  } },
  h('h1', {}, '登录 Gitrepo'),
  h('p', { class: 'hint' }, '使用部署时设置的管理令牌（GITREPO_ADMIN_TOKEN），或具有 admin 权限的令牌。令牌只保存在当前标签页。'),
  field('管理令牌', input), h('p'), h('button', { type: 'submit' }, '登录'), err);
  app.replaceChildren(h('div', { class: 'login' }, form));
  input.focus();
}

// -------------------------------------------------------------------- repos

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
      h('td', {}, h('a', { href: '#/repo/' + r.slug }, r.slug)),
      h('td', {}, badge(r.state), r.busy ? h('span', { class: 'muted' }, ' …') : null),
      h('td', {}, `${synced}/${r.remotes.length}`),
      h('td', { class: 'nowrap' }, r.remotes.length ? (never ? '从未' : ago(oldest)) : '—'),
      h('td', {}, copyable(r.clone_url)));
  });

  const nodes = [h('div', { class: 'spread' }, h('h1', {}, '仓库'), h('button', { class: 'secondary small', onclick: rerender }, '刷新'))];
  if (!targets.length || !bindings.length) nodes.push(quickStart(targets, bindings));
  nodes.push(table(['仓库', '状态', '已同步镜像', '上次全部同步', 'Push / Clone 地址'], rows, '还没有仓库。push 到 ' + location.origin + '/<组织>/<仓库>.git 即可自动创建。'));

  const form = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    const r = await api('POST', '/api/repos', { slug: d.slug, visibility: d.visibility, bindings: d.bindings });
    location.hash = '#/repo/' + r.slug;
    return '已创建 ' + r.slug;
  }) },
  field('仓库（组织/名称）', h('input', { name: 'slug', placeholder: 'acme/demo', required: true, pattern: '[A-Za-z0-9][A-Za-z0-9._\\-]*/[A-Za-z0-9][A-Za-z0-9._\\-]*' })),
  field('可见性', select('visibility', [['private', '私有'], ['public', '公开（允许匿名读取，镜像也设为公开）']], 'private')),
  h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'bindings', checked: true }), '应用备份规则'),
  h('div', {}, h('button', { type: 'submit' }, '创建')));
  nodes.push(h('h2', {}, '手动创建仓库'), h('div', { class: 'panel' }, h('p', { class: 'hint' }, '也可以不在这里创建：用有 create 权限的令牌直接 push 到新地址，仓库会自动创建。'), form));

  const importForm = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    if (d.force && !confirm('Hub 上已有的分支和标签会被替换（先备份到 refs/backup/）。继续？')) return false;
    const res = await api('POST', '/api/repos/' + d.slug.split('/').map(enc).join('/') + '/import', { target: d.target, namespace: d.namespace, name: d.name, force: d.force });
    location.hash = '#/repo/' + res.repo.slug;
    const warn = (res.import.warnings || []).length ? '（有警告，见同步记录）' : '';
    return `已从 ${res.source} 导入 ${res.import.refs} 个 ref、${res.import.lfs_objects} 个 LFS 对象${warn}`;
  }) },
  field('Hub 仓库（组织/名称）', h('input', { name: 'slug', placeholder: 'acme/demo', required: true, pattern: '[A-Za-z0-9][A-Za-z0-9._\\-]*/[A-Za-z0-9][A-Za-z0-9._\\-]*' })),
  field('从哪个平台导入', select('target', targets.map((t) => [t.name, `${t.name}（${t.provider}${t.namespace ? ' · ' + t.namespace : ''}）`]))),
  field('命名空间（可选）', h('input', { name: 'namespace', placeholder: '默认使用凭据上的命名空间' })),
  field('平台上的仓库名（可选）', h('input', { name: 'name', placeholder: '默认与 Hub 同名' })),
  h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'force' }), '替换 Hub 上已有的内容'),
  h('div', {}, h('button', { type: 'submit', disabled: !targets.length }, '导入')));
  nodes.push(h('h2', {}, '从已有镜像导入'), h('div', { class: 'panel' },
    h('p', { class: 'hint' }, '把 GitHub、GitLab 等平台上已有的仓库（包括 LFS 内容）作为 Hub 的初始内容，然后同步到其他镜像。Hub 数据丢失时，也用它从任一镜像恢复。'),
    importForm));
  app.replaceChildren(...nodes);
}

function quickStart(targets, bindings) {
  const done = (ok) => (ok ? '✓ ' : '');
  return h('div', { class: 'panel' },
    h('h2', { class: 'flush-top' }, '开始使用'),
    h('ol', { class: 'steps' },
      h('li', {}, done(targets.length), h('a', { href: '#/targets' }, '填入平台凭据'), '：GitHub、GitLab、Gitea/Forgejo 或任意 Git 服务器。'),
      h('li', {}, done(bindings.length), h('a', { href: '#/bindings' }, '添加备份规则'), '：例如所有组织（*）的新仓库都备份到 GitHub 和 GitLab。'),
      h('li', {}, h('a', { href: '#/tokens' }, '创建访问令牌'), '（create 权限），作为 git 的 HTTPS 密码。'),
      h('li', {}, 'push：', h('code', {}, `git push ${location.origin}/<组织>/<仓库>.git main`))));
}

async function viewRepo(org, name) {
  const slug = org + '/' + name;
  const base = '/api/repos/' + enc(org) + '/' + enc(name);
  const [repo, runs, targets, backups, audit, releases] = await Promise.all([api('GET', base), api('GET', base + '/runs?limit=30'), api('GET', '/api/targets'), api('GET', base + '/backups'),
    api('GET', '/api/audit?limit=10&repo=' + enc(slug)), api('GET', base + '/releases')]);

  const syncNow = (overwrite) => action(async () => {
    if (overwrite && !confirm('以 Hub 为准覆盖所有分叉的镜像 ref？\n被覆盖的镜像内容会先备份到 Hub 的 refs/backup/ 下。')) return false;
    const res = await api('POST', base + '/sync', { wait: true, overwrite });
    rerender();
    const bad = (res.events || []).filter((e) => e.type === 'remote' && e.state !== 'synced' && e.state !== 'paused');
    return bad.length ? `完成，${bad.length} 个镜像未同步` : '已同步';
  });

  const remoteRows = repo.remotes.map((m) => {
    const path = base + '/remotes/' + m.id;
    const details = [];
    if (m.needs_adoption) {
      details.push(h('div', { class: 'row' }, h('span', { class: 'muted' }, '镜像仓库里已有 Gitrepo 没写过的内容。确认是正确的仓库后再接管：'),
        h('button', { class: 'small', onclick: action(async () => {
          if (!confirm(`确认 ${m.url} 是 ${slug} 的镜像，交给 Gitrepo 管理？\n已有分支只会快进或报告分叉，不会被覆盖。`)) return false;
          await api('PATCH', path, { adopt: true });
          rerender();
        }, '已接管，正在同步') }, '接管')));
    }
    if (m.last_error) details.push(h('div', { class: 'error-text' }, `${m.last_error}（连续失败 ${m.consecutive_failures || 0} 次，${m.next_retry_at ? '下次重试 ' + ago(m.next_retry_at) : ''}）`));
    if (m.divergences && m.divergences.length) {
      details.push(h('ul', { class: 'div-list' }, m.divergences.map((d) => h('li', {},
        h('code', {}, d.ref), ' ', d.reason, h('span', { class: 'muted' }, ` 镜像 ${short(d.remote_sha)} · Hub ${short(d.hub_sha)} · 上次推送 ${short(d.expected_sha)}`)))));
      details.push(h('div', { class: 'muted' }, '解决：把镜像的提交合并后 push 到 Hub，或点击上方“以 Hub 为准覆盖分叉”。'));
    }
    return h('tr', {},
      h('td', {}, h('strong', {}, m.label), details),
      h('td', {}, badge(m.enabled ? m.state : 'paused')),
      h('td', { class: 'nowrap' }, ago(m.last_success_at)),
      h('td', { class: 'nowrap' }, m.on_divergence === 'overwrite' ? '覆盖' : '告警'),
      h('td', { class: 'mono' }, m.url),
      h('td', { class: 'actions' },
        h('button', { class: 'secondary small', onclick: action(async () => { await api('PATCH', path, { enabled: !m.enabled }); rerender(); }, m.enabled ? '已暂停' : '已恢复') }, m.enabled ? '暂停' : '恢复'), ' ',
        h('button', { class: 'secondary small', title: '切换分叉策略', onclick: action(async () => {
          const next = m.on_divergence === 'overwrite' ? 'alert' : 'overwrite';
          if (next === 'overwrite' && !confirm('分叉时自动以 Hub 为准覆盖该镜像（会先备份）？')) return false;
          await api('PATCH', path, { on_divergence: next });
          rerender();
        }, '已更新') }, m.on_divergence === 'overwrite' ? '改为告警' : '改为覆盖'), ' ',
        h('button', { class: 'danger small', onclick: action(async () => {
          if (!confirm(`停止备份到 ${m.label}？（远端仓库不会被删除）`)) return false;
          await api('DELETE', path);
          rerender();
        }, '已移除') }, '移除')));
  });

  const runRows = runs.map((r) => {
    const dur = r.finished_at && r.started_at ? ((new Date(r.finished_at) - new Date(r.started_at)) / 1000).toFixed(1) + 's' : '—';
    return h('tr', {}, h('td', { class: 'nowrap' }, ago(r.started_at)), h('td', {}, r.remote), h('td', {}, r.trigger), h('td', {}, badge(r.status)),
      h('td', {}, r.refs_changed), h('td', {}, dur), h('td', { class: 'error-text' }, r.error || ''));
  });

  const addForm = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    await api('POST', base + '/remotes', d);
    rerender();
    return '已添加，正在首次同步';
  }) },
  field('平台凭据', select('target', targets.map((t) => [t.name, `${t.name}（${t.provider}${t.namespace ? ' · ' + t.namespace : ''}）`]))),
  field('命名空间（可选）', h('input', { name: 'namespace', placeholder: '默认使用凭据上的命名空间' })),
  field('远端仓库名（可选）', h('input', { name: 'name', placeholder: name })),
  field('完整 URL（可选，覆盖推导）', h('input', { name: 'url', placeholder: 'ssh://git@git.example.com/x/y.git' })),
  field('分叉时', select('on_divergence', [['alert', '告警，不覆盖'], ['overwrite', '备份后覆盖']], 'alert')),
  h('div', {}, h('button', { type: 'submit', disabled: !targets.length }, '添加镜像')));

  app.replaceChildren(
    h('div', { class: 'spread' },
      h('h1', {}, slug, ' ', badge(repo.state)),
      h('div', { class: 'row' },
        h('button', { onclick: syncNow(false) }, '立即同步'),
        h('button', { class: 'secondary', onclick: syncNow(true) }, '以 Hub 为准覆盖分叉'),
        h('button', { class: 'secondary', onclick: rerender }, '刷新'))),
    h('div', { class: 'panel' }, h('dl', { class: 'kv' },
      h('dt', {}, 'Push / Clone'), h('dd', {}, copyable(repo.clone_url)),
      repo.ssh_url ? [h('dt', {}, 'SSH'), h('dd', {}, copyable(repo.ssh_url))] : null,
      h('dt', {}, '可见性'), h('dd', {}, { public: '公开', internal: '内部', private: '私有' }[repo.visibility] || repo.visibility, ' ',
        h('button', { class: 'secondary small', onclick: action(async () => {
          const next = repo.visibility === 'public' ? 'private' : 'public';
          const label = next === 'public' ? '公开' : '私有';
          if (!confirm(`把 ${slug} 改为${label}？\n跟随仓库可见性的镜像也会通过平台 API 一起修改。`)) return false;
          const res = await api('PATCH', base, { visibility: next });
          rerender();
          const bad = res.mirrors.filter((m) => m.state === 'failed' || m.state === 'unsupported');
          if (bad.length) throw new Error(`已改为${label}，但这些镜像没有改：` + bad.map((m) => `${m.mirror}（${m.detail}）`).join('；'));
          return `已改为${label}` + (res.mirrors.length ? `，${res.mirrors.filter((m) => m.state === 'changed').length} 个镜像已同步修改` : '');
        }) }, repo.visibility === 'public' ? '改为私有' : '改为公开')),
      h('dt', {}, '默认分支'), h('dd', {}, repo.default_branch || '（空仓库）'),
      (repo.protected || []).length ? [h('dt', {}, '受保护'), h('dd', {}, repo.protected.map((p, i) => [i ? '；' : '', h('code', {}, p.ref), ' ', protectLabel(p)]),
        ' ', h('a', { href: '#/protect' }, '管理'))] : null,
      h('dt', {}, '添加远端'), h('dd', {}, h('code', {}, `git remote add hub ${repo.clone_url}`)))),
    h('h2', {}, '镜像'),
    table(['镜像', '状态', '上次成功', '分叉时', 'URL', ''], remoteRows, '还没有镜像。'),
    h('h2', {}, '添加镜像'),
    h('div', { class: 'panel' }, targets.length ? addForm : h('p', {}, '请先', h('a', { href: '#/targets' }, '添加平台凭据'), '。')),
    h('h2', {}, 'Release'),
    h('p', { class: 'hint' }, '在这里发布一次，Gitrepo 会把说明和附件发布到所有 GitHub、GitLab、Gitea/Forgejo、Gitee 镜像；标签同步到某个镜像后才会在那里创建。平台上已有的、不是 Gitrepo 建的同名 Release 不会被覆盖（显示"冲突"）。'),
    releaseTable(base, releases),
    h('details', { class: 'panel' }, h('summary', {}, '新建 Release'), releaseForm(base, repo)),
    h('h2', {}, '备份'),
    h('p', { class: 'hint' }, '被强推覆盖、删除或导入替换的历史会在 Hub 上隐藏保留一段时间（默认 30 天），clone 看不到。泄露了密钥时，请轮换密钥并清除备份。'),
    table(['时间', '来源', '原 ref', '提交', '说明', '到期', ''], backups.map((b) => h('tr', {},
      h('td', { class: 'nowrap' }, ago(b.created_at)),
      h('td', {}, b.remote || (b.source === 'hub' ? 'Hub' : b.source)),
      h('td', { class: 'mono' }, b.original),
      h('td', { class: 'mono' }, short(b.sha)),
      h('td', {}, b.subject || ''),
      h('td', { class: 'nowrap' }, fmtDay(b.expires_at)),
      h('td', { class: 'actions' },
        h('button', { class: 'secondary small', onclick: action(async () => {
          const res = await api('POST', base + '/backups/restore', { backup: b.ref });
          rerender();
          return `已恢复为分支 ${res.branch}`;
        }) }, '恢复为分支'), ' ',
        h('button', { class: 'danger small', onclick: action(async () => {
          if (window.prompt(`彻底清除这个备份（Hub 上的内容会被删除，无法恢复）？输入 ${slug} 确认`) !== slug) return false;
          await api('POST', base + '/backups/purge', { backups: [b.ref], lfs: true });
          rerender();
        }, '已清除') }, '清除')))), '没有备份。'),
    h('h2', {}, '同步记录'),
    table(['时间', '镜像', '触发', '结果', '变更', '耗时', '错误'], runRows, '暂无记录。'),
    h('div', { class: 'spread' }, h('h2', {}, '操作记录'), h('a', { href: '#/audit?repo=' + enc(slug) }, '查看全部')),
    auditTable(audit.entries, false),
    h('h2', {}, '危险操作'),
    h('div', { class: 'panel' }, h('div', { class: 'spread' },
      h('span', { class: 'muted' }, '注销后 Hub 上的数据移入 trash 目录，各镜像不受影响。'),
      h('button', { class: 'danger', onclick: action(async () => {
        if (window.prompt(`输入 ${slug} 确认注销`) !== slug) return false;
        await api('DELETE', base);
        location.hash = '#/';
      }, '已注销') }, '注销仓库'))));
}

// ------------------------------------------------------------------ targets

const PROVIDERS = [
  ['github', 'GitHub / GitHub Enterprise'],
  ['gitlab', 'GitLab（gitlab.com 或自建）'],
  ['gitea', 'Gitea / Forgejo'],
  ['gitee', 'Gitee（码云）'],
  ['generic', '其他 Git 服务器（无 API）'],
];

async function viewTargets() {
  const targets = await api('GET', '/api/targets');
  const editor = h('div');

  const rows = targets.map((t) => h('tr', {},
    h('td', {}, h('strong', {}, t.name)),
    h('td', {}, t.provider),
    h('td', {}, t.namespace || '—'),
    h('td', { class: 'mono' }, t.url_template || t.base_url || '（默认）'),
    h('td', {}, t.credential_kind === 'github-app' ? `GitHub App ${t.app_id}（安装 ${t.installation_id}）` : t.credential_kind,
      t.credential_username ? ` (${t.credential_username})` : ''),
    h('td', {}, t.expires_at ? fmtDay(t.expires_at) : '—', t.expires_detected ? h('span', { class: 'muted' }, '（自动识别）') : ''),
    h('td', { class: 'actions' },
      t.provider !== 'generic' ? h('button', { class: 'secondary small', onclick: action(async () => {
        const res = await api('POST', '/api/targets/' + enc(t.name) + '/check');
        if (!res.ok) throw new Error('验证失败：' + res.error);
        return '验证通过：' + res.identity;
      }) }, '验证') : null, ' ',
      h('button', { class: 'secondary small', onclick: () => editor.replaceChildren(editTarget(t, editor)) }, '修改'), ' ',
      h('button', { class: 'danger small', onclick: action(async () => {
        if (!confirm(`删除平台凭据 ${t.name}？`)) return false;
        await api('DELETE', '/api/targets/' + enc(t.name));
        rerender();
      }, '已删除') }, '删除'))));

  app.replaceChildren(
    h('h1', {}, '平台凭据'),
    h('p', { class: 'hint' }, '每个平台账号填一次。凭据用主密钥加密后入库，界面和 API 都不会再显示。'),
    table(['名称', '平台', '命名空间', '地址', '凭据', '过期', ''], rows, '还没有平台凭据。'),
    editor,
    h('h2', {}, '添加平台凭据'),
    h('div', { class: 'panel' }, targetForm()));
}

function targetForm() {
  const provider = select('provider', PROVIDERS, 'github');
  const KINDS = [['token', '访问令牌'], ['github-app', 'GitHub App（推荐给组织）'], ['basic', '用户名 + 密码'], ['ssh', 'SSH 私钥'], ['none', '无（匿名）']];
  const kind = select('kind', KINDS, 'token');
  const dyn = h('div', { class: 'contents' });
  const help = h('p', { class: 'hint wide' });

  const HELP = {
    github: 'Fine-grained token：Contents 读写；自动建仓还需 Administration 读写。也可以用 GitHub App：不绑定个人，令牌每小时自动换新。命名空间填拥有镜像的用户或组织。',
    gitlab: 'Access token：api + write_repository。命名空间填用户或 group 路径（可含子 group）。自建实例填写站点地址。',
    gitea: 'Access token：repository 读写；在组织下建仓需 organization 写权限。站点地址必填。',
    gitee: '私人令牌：勾选 projects 权限。命名空间填个人空间或组织路径；git 用户名会自动从令牌查出。',
    generic: '没有 API，仓库需事先存在。URL 模板中 {namespace} 和 {name} 会被替换。',
  };

  function update() {
    const p = provider.value;
    help.textContent = HELP[p];
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
      nodes.push(field('安装 ID（可选，默认按命名空间查找）', h('input', { name: 'installation_id', inputmode: 'numeric', pattern: '[0-9]*' })));
      nodes.push(field('私钥（App 设置页生成的 .pem）', keyArea, 'wide'));
      nodes.push(field('或选择 .pem 文件', h('input', { type: 'file', accept: '.pem', onchange: async (ev) => {
        const f = ev.target.files[0];
        if (f) keyArea.value = await f.text();
      } })));
      nodes.push(h('p', { class: 'hint wide' }, 'App 需要的仓库权限：Contents 读写、Metadata 只读；自动建仓还需 Administration 读写；同步 Release 需 Contents 读写；推送 .github/workflows 需 Workflows 读写。安装时选 All repositories，新建的镜像才在 App 的范围内。只能在组织下自动建仓。'));
      dyn.replaceChildren(...nodes);
      return;
    }
    if (k === 'basic' || (k === 'token' && p === 'generic')) nodes.push(field('用户名' + (k === 'token' ? '（可选）' : ''), h('input', { name: 'username', required: k === 'basic' })));
    if (k === 'ssh') {
      nodes.push(field('SSH 私钥', h('textarea', { name: 'secret', required: true, placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----' }), 'wide'));
      nodes.push(field('known_hosts（可选，填写后严格校验主机密钥）', h('textarea', { name: 'ssh_known_hosts' }), 'wide'));
    } else if (k !== 'none') {
      nodes.push(field(k === 'basic' ? '密码' : '访问令牌', h('input', { name: 'secret', type: 'password', required: true, autocomplete: 'off' })));
    }
    nodes.push(field('凭据过期日（可选，用于提醒；GitHub、GitLab 令牌会自动识别）', h('input', { name: 'expires_at', type: 'date' })));
    dyn.replaceChildren(...nodes);
  }
  provider.addEventListener('change', update);
  kind.addEventListener('change', update);

  const form = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
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
  }) },
  help,
  field('名称', h('input', { name: 'name', required: true, placeholder: 'github', pattern: '[A-Za-z0-9._\\-]+' })),
  field('平台', provider),
  field('凭据类型', kind),
  dyn,
  field('镜像可见性', select('visibility', [['', '跟随仓库'], ['private', '始终私有'], ['public', '始终公开']], '')),
  h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'auto_create', checked: true }), '自动创建缺失的远端仓库'),
  h('div', { class: 'wide' }, h('button', { type: 'submit' }, '添加')));
  update();
  return form;
}

function editTarget(t, container) {
  const close = () => container.replaceChildren();
  const isSSH = t.credential_kind === 'ssh' || t.credential_kind === 'github-app';
  const form = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
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
    close();
    rerender();
    return '已保存';
  }) },
  field('命名空间', h('input', { name: 'namespace', value: t.namespace || '' })),
  t.provider === 'generic'
    ? field('URL 模板', h('input', { name: 'url_template', value: t.url_template || '' }), 'wide')
    : field('站点地址', h('input', { name: 'base_url', value: t.base_url || '' })),
  field('单文件上限 MiB（0 = 平台默认）', h('input', { name: 'max_file_size', type: 'number', min: '0', value: t.max_file_size ? Math.round(t.max_file_size / 1048576) : '' })),
  field('LFS 对象上限 MiB（0 = 平台默认）', h('input', { name: 'max_lfs_object_size', type: 'number', min: '0', value: t.max_lfs_object_size ? Math.round(t.max_lfs_object_size / 1048576) : '' })),
  field(t.expires_detected ? '凭据过期日（从平台读取）' : '凭据过期日（清空则从平台读取）', h('input', { name: 'expires_at', type: 'date', value: t.expires_at ? t.expires_at.slice(0, 10) : '' })),
  t.credential_kind === 'none' ? null : field(t.credential_kind === 'github-app' ? '新 App 私钥（留空不变）' : (isSSH ? '新 SSH 私钥（留空不变）' : '新令牌 / 密码（留空不变）'),
    isSSH ? h('textarea', { name: 'secret' }) : h('input', { name: 'secret', type: 'password', autocomplete: 'off' }), isSSH ? 'wide' : ''),
  h('div', { class: 'row wide' }, h('button', { type: 'submit' }, '保存'), h('button', { type: 'button', class: 'secondary', onclick: close }, '取消')));
  return h('div', {}, h('h2', {}, '修改 ', t.name), h('div', { class: 'panel' }, form));
}

// ----------------------------------------------------------------- bindings

async function viewBindings() {
  const [bindings, targets] = await Promise.all([api('GET', '/api/bindings'), api('GET', '/api/targets')]);
  const rows = bindings.map((b) => h('tr', {},
    h('td', {}, b.org === '*' ? '所有组织（*）' : b.org),
    h('td', {}, b.target),
    h('td', {}, b.namespace || h('span', { class: 'muted' }, '凭据默认')),
    h('td', { class: 'mono' }, b.name_template),
    h('td', {}, b.on_divergence === 'overwrite' ? '备份后覆盖' : '告警'),
    h('td', { class: 'actions' }, h('button', { class: 'danger small', onclick: action(async () => {
      if (!confirm('删除这条规则？已有仓库的镜像不受影响。')) return false;
      await api('DELETE', '/api/bindings/' + b.id);
      rerender();
    }, '已删除') }, '删除'))));
  const form = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    const res = await api('POST', '/api/bindings', d);
    rerender();
    if (res.skipped && res.skipped.length) throw new Error(`已添加；${res.skipped.length} 个仓库因镜像名冲突被跳过：\n` + res.skipped.join('\n'));
    return d.apply_existing ? `已添加，并应用到 ${res.applied_to_repos} 个已有仓库` : '已添加';
  }) },
  field('组织（* 表示全部）', h('input', { name: 'org', value: '*', required: true })),
  field('平台凭据', select('target', targets.map((t) => [t.name, `${t.name}（${t.provider}）`]))),
  field('命名空间（可选）', h('input', { name: 'namespace', placeholder: '默认使用凭据上的命名空间' })),
  field('镜像仓库名', h('input', { name: 'name_template', value: '{name}', required: true, title: '{org} 和 {name} 是 Hub 仓库的组织和名称' })),
  field('分叉时', select('on_divergence', [['alert', '告警，不覆盖'], ['overwrite', '备份后覆盖']], 'alert')),
  h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'apply_existing' }), '同时应用到已有仓库'),
  h('div', {}, h('button', { type: 'submit', disabled: !targets.length }, '添加规则')));
  app.replaceChildren(
    h('h1', {}, '备份规则'),
    h('p', { class: 'hint' }, '新仓库创建时（包括 push 自动创建），按规则自动添加镜像。'),
    h('p', { class: 'hint' }, '多个组织备份到同一个命名空间时，把镜像仓库名设为 {org}-{name} 以免重名；同一个镜像位置只能属于一个 Hub 仓库。'),
    table(['组织', '平台凭据', '命名空间', '镜像仓库名', '分叉时', ''], rows, '还没有规则。'),
    h('h2', {}, '添加规则'),
    h('div', { class: 'panel' }, targets.length ? form : h('p', {}, '请先', h('a', { href: '#/targets' }, '添加平台凭据'), '。')));
}

// ------------------------------------------------------------------ protect

function protectLabel(p) {
  return [p.deny_force ? '禁止强推' : '', p.deny_delete ? '禁止删除' : ''].filter(Boolean).join('、');
}

async function viewProtect() {
  const rules = await api('GET', '/api/protections');
  const rows = rules.map((p) => h('tr', {},
    h('td', { class: 'mono' }, p.repos === '*' ? '所有仓库（*）' : p.repos),
    h('td', { class: 'mono' }, p.ref),
    h('td', {}, protectLabel(p)),
    h('td', { class: 'actions' }, h('button', { class: 'danger small', onclick: action(async () => {
      if (!confirm(`取消对 ${p.repos} 中 ${p.ref} 的保护？`)) return false;
      await api('DELETE', '/api/protections/' + p.id);
      rerender();
    }, '已删除') }, '删除'))));
  const form = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    await api('POST', '/api/protections', { repos: d.repos, ref: d.ref, deny_force: d.deny_force, deny_delete: d.deny_delete });
    rerender();
    return '已添加';
  }) },
  field('仓库（* 表示全部，也可以是 acme/*）', h('input', { name: 'repos', value: '*', required: true })),
  field('分支（如 main、release/*；标签写 refs/tags/v*）', h('input', { name: 'ref', value: 'main', required: true })),
  h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'deny_force', checked: true }), '禁止强推（改写历史）'),
  h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'deny_delete', checked: true }), '禁止删除'),
  h('div', {}, h('button', { type: 'submit' }, '添加')));
  app.replaceChildren(
    h('h1', {}, '分支保护'),
    h('p', { class: 'hint' }, '受保护的分支在 Hub 上不能被强推或删除（HTTPS 和 SSH 都一样），镜像也就不会被改写。受保护的标签不能被移动或删除。* 不跨越 /。'),
    h('p', { class: 'hint' }, '确实需要改写时（例如清除误提交的密钥），先删除对应规则，完成后再加回来；这些操作都记在审计日志里。'),
    table(['仓库', '分支 / 标签', '规则', ''], rows, '还没有规则。'),
    h('h2', {}, '添加规则'),
    h('div', { class: 'panel' }, form));
}

// ------------------------------------------------------------------- tokens

async function viewTokens() {
  const [tokens, keys, sshInfo] = await Promise.all([api('GET', '/api/tokens'), api('GET', '/api/ssh-keys'), api('GET', '/api/ssh')]);
  const created = h('div');
  const rows = tokens.map((t) => h('tr', {},
    h('td', {}, t.name), h('td', {}, t.scopes.join(', ')), h('td', {}, (t.repos || []).join(', ') || '全部'),
    h('td', {}, ago(t.last_used_at)),
    h('td', { class: 'actions' }, h('button', { class: 'danger small', onclick: action(async () => {
      if (!confirm(`吊销令牌 ${t.name}？`)) return false;
      await api('DELETE', '/api/tokens/' + t.id);
      rerender();
    }, '已吊销') }, '吊销'))));
  const form = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    const t = await api('POST', '/api/tokens', { name: d.name, scopes: [d.scope], repos: list(d.repos) });
    await viewTokens();
    document.querySelector('#created').replaceChildren(h('div', { class: 'panel' },
      h('strong', {}, `令牌 ${t.name} 已创建，只显示这一次：`),
      h('div', { class: 'secret-box' }, t.token), copyable(t.token),
      h('p', { class: 'hint' }, '作为 git 的 HTTPS 密码使用（用户名任意），例如：'),
      h('code', {}, `git push https://x:${t.token.slice(0, 8)}…@${location.host}/acme/demo.git main`)));
    return false;
  }) },
  field('名称', h('input', { name: 'name', required: true, placeholder: 'laptop', pattern: '[A-Za-z0-9._\\-]+' })),
  field('权限', select('scope', [['read', 'read：clone / fetch'], ['write', 'write：另外可以 push'], ['create', 'create：另外可以 push 新地址建仓'], ['admin', 'admin：另外可以管理']], 'create')),
  field('仓库范围（可选）', h('input', { name: 'repos', placeholder: 'acme/*, tools/cli（默认全部）' })),
  h('div', {}, h('button', { type: 'submit' }, '创建令牌')));
  created.id = 'created';

  const keyRows = keys.map((k) => h('tr', {},
    h('td', {}, k.name), h('td', {}, k.type), h('td', { class: 'mono' }, k.fingerprint),
    h('td', {}, k.scopes.join(', ')), h('td', {}, (k.repos || []).join(', ') || '全部'), h('td', {}, ago(k.last_used_at)),
    h('td', { class: 'actions' }, h('button', { class: 'danger small', onclick: action(async () => {
      if (!confirm(`删除 SSH 公钥 ${k.name}？`)) return false;
      await api('DELETE', '/api/ssh-keys/' + k.id);
      rerender();
    }, '已删除') }, '删除'))));
  const keyForm = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    await api('POST', '/api/ssh-keys', { name: d.name, public_key: d.public_key, scopes: [d.scope], repos: list(d.repos) });
    rerender();
    return '已添加';
  }) },
  field('名称', h('input', { name: 'name', required: true, placeholder: 'laptop', pattern: '[A-Za-z0-9._\\-]+' })),
  field('权限', select('scope', [['read', 'read'], ['write', 'write'], ['create', 'create']], 'create')),
  field('仓库范围（可选）', h('input', { name: 'repos', placeholder: 'acme/*（默认全部）' })),
  field('公钥（~/.ssh/id_ed25519.pub 的内容）', h('textarea', { name: 'public_key', required: true, placeholder: 'ssh-ed25519 AAAA… you@laptop' }), 'wide'),
  h('div', {}, h('button', { type: 'submit' }, '添加公钥')));
  const sshSection = sshInfo.enabled
    ? [h('p', { class: 'hint' }, '地址示例：', h('code', {}, sshInfo.example_url), '。主机指纹 ', h('code', {}, sshInfo.fingerprint), '，首次连接时请核对。'),
      table(['名称', '类型', '指纹', '权限', '仓库范围', '上次使用', ''], keyRows, '还没有公钥。'),
      h('h2', {}, '添加 SSH 公钥'),
      h('div', { class: 'panel' }, keyForm)]
    : [h('p', { class: 'hint' }, 'SSH 未启用。在服务端设置 ssh.listen（或环境变量 GITREPO_SSH_LISTEN，例如 :2222）后即可通过 SSH push 和 clone。')];

  app.replaceChildren(
    h('h1', {}, '访问令牌'),
    h('p', { class: 'hint' }, '给开发者、CI 和部署平台使用。只保存哈希，丢失后只能重新创建。'),
    created,
    table(['名称', '权限', '仓库范围', '上次使用', ''], rows, '还没有令牌。'),
    h('h2', {}, '创建令牌'),
    h('div', { class: 'panel' }, form),
    h('h2', {}, 'SSH 公钥'),
    ...sshSection);
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

async function viewWebhooks() {
  const hooks = await api('GET', '/api/webhooks');
  const rows = hooks.map((w) => h('tr', {},
    h('td', {}, w.name, w.signed ? h('span', { class: 'muted' }, ' · 已签名') : null),
    h('td', {}, w.format), h('td', { class: 'mono' }, w.host),
    h('td', {}, w.events.join(', ')), h('td', {}, (w.repos || []).join(', ') || '全部'),
    h('td', {}, w.enabled ? '启用' : '停用'),
    h('td', {}, w.last_delivery_at ? `${w.last_status}（${ago(w.last_delivery_at)}）` : '从未'),
    h('td', { class: 'actions' },
      h('button', { class: 'secondary small', onclick: action(async () => {
        const res = await api('POST', `/api/webhooks/${w.id}/test`);
        rerender();
        if (!res.ok) throw new Error('投递失败：' + res.error);
        return '测试投递成功';
      }) }, '测试'), ' ',
      h('button', { class: 'secondary small', onclick: action(async () => { await api('PATCH', `/api/webhooks/${w.id}`, { enabled: !w.enabled }); rerender(); }) }, w.enabled ? '停用' : '启用'), ' ',
      h('button', { class: 'danger small', onclick: action(async () => {
        if (!confirm(`删除通知 ${w.name}？`)) return false;
        await api('DELETE', `/api/webhooks/${w.id}`);
        rerender();
      }, '已删除') }, '删除'))));

  const form = h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    const events = d.events && d.events.length === EVENTS.length ? ['*'] : (d.events || []);
    if (!events.length) throw new Error('至少选择一个事件');
    await api('POST', '/api/webhooks', { name: d.name, url: d.url, format: d.format, events, repos: list(d.repos), secret: d.secret });
    rerender();
    return '已添加，可以点“测试”验证';
  }) },
  field('名称', h('input', { name: 'name', required: true, placeholder: 'oncall', pattern: '[A-Za-z0-9._\\-]+' })),
  field('格式', select('format', [['json', 'JSON（带 HMAC 签名，给程序用）'], ['wecom', '企业微信机器人'], ['feishu', '飞书机器人'], ['dingtalk', '钉钉机器人'], ['slack', 'Slack']], 'json')),
  field('URL（加密保存）', h('input', { name: 'url', type: 'url', required: true, placeholder: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…' }), 'wide'),
  h('div', { class: 'wide row' }, EVENTS.map(([v, text]) => h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'ev_' + v, value: v, 'data-group': 'events', checked: v !== 'push' }), text))),
  field('仓库范围（可选）', h('input', { name: 'repos', placeholder: 'acme/*（默认全部）' })),
  field('签名密钥（JSON 格式可选）', h('input', { name: 'secret', type: 'password', autocomplete: 'off' })),
  h('div', {}, h('button', { type: 'submit' }, '添加通知')));

  app.replaceChildren(
    h('h1', {}, '通知'),
    h('p', { class: 'hint' }, '分叉、失败等告警只在状态变化时发送一次；sync.completed 只在镜像确实有更新时发送，载荷包含 ref 和 SHA，可用于触发下游构建。'),
    table(['名称', '格式', '目标', '事件', '仓库', '状态', '最近投递', ''], rows, '还没有通知。'),
    h('h2', {}, '添加通知'),
    h('div', { class: 'panel' }, form));
}

// -------------------------------------------------------------------- audit

const VIA_LABEL = { api: 'API', http: 'HTTPS', ssh: 'SSH' };

function auditRow(e, showRepo) {
  let what;
  if (e.path) {
    what = h('code', {}, e.action.split(' ')[0] + ' ' + e.path);
  } else {
    what = [h('code', {}, e.action), showRepo && e.repo ? [' ', h('a', { href: '#/repo/' + e.repo.split('/').map(enc).join('/') }, e.repo)] : null];
  }
  let result;
  if (!e.status) result = h('span', { class: 'badge synced' }, '已接受');
  else if (e.status < 400) result = h('span', { class: 'badge synced' }, String(e.status));
  else result = h('span', { class: 'badge failed' }, String(e.status));
  return h('tr', {},
    h('td', { class: 'nowrap', title: fmtDate(e.at) }, fmtDate(e.at)),
    h('td', {}, e.actor),
    h('td', {}, VIA_LABEL[e.via] || e.via),
    h('td', { class: 'mono' }, e.client_addr || '—'),
    h('td', {}, what),
    h('td', {}, result),
    h('td', {}, e.detail || ''));
}

function auditTable(entries, showRepo) {
  return table(['时间', '操作者', '方式', '来源地址', '操作', '结果', '说明'], entries.map((e) => auditRow(e, showRepo)), '暂无记录。');
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
  const more = h('button', { class: 'secondary', hidden: !next, onclick: action(async () => {
    const p = await api('GET', query({ before: String(next) }));
    for (const e of p.entries) tbody.append(auditRow(e, true));
    next = p.next || 0;
    more.hidden = !next;
    return false;
  }) }, '加载更早的记录');

  const form = h('form', { class: 'grid', onsubmit: (ev) => {
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
  h('div', { class: 'row' }, h('button', { type: 'submit' }, '筛选'), ' ', h('a', { href: '#/audit' }, '清除筛选')));

  app.replaceChildren(
    h('h1', {}, '审计日志'),
    h('p', { class: 'hint' }, '记录所有修改类 API 调用（包括被拒绝的）和 Hub 接受的每次 push：谁、何时、从哪个地址、做了什么。请求内容（凭据等）从不记录。默认保留 365 天（audit.retention）。'),
    h('div', { class: 'panel' }, form),
    wrap,
    h('div', { class: 'row' }, more));
}

// ----------------------------------------------------------------- releases

function releaseTable(base, releases) {
  const path = (r) => base + '/releases/' + enc(r.tag);
  return table(['标签', '标题', '附件', '镜像', '创建', ''], releases.map((r) => h('tr', {},
    h('td', { class: 'mono' }, r.tag),
    h('td', {}, r.name, r.prerelease ? h('span', { class: 'muted' }, '（预发布）') : null),
    h('td', {}, r.assets.map((a) => h('div', {}, h('a', { href: a.url, download: a.name, onclick: action(async (ev) => {
      // The API needs the token, so fetch the file instead of following the link.
      ev.preventDefault();
      const res = await fetch(a.url.replace(/^https?:\/\/[^/]+/, ''), { headers: { Authorization: 'Bearer ' + token } });
      if (!res.ok) throw new Error('下载失败：' + res.status);
      const blob = await res.blob();
      const link = h('a', { href: URL.createObjectURL(blob), download: a.name });
      link.click();
      setTimeout(() => URL.revokeObjectURL(link.href), 10000);
      return false;
    }) }, a.name), h('span', { class: 'muted' }, ' ' + fmtSize(a.size))))),
    h('td', {}, r.mirrors.map((m) => h('div', { title: m.error || '' }, h('span', { class: 'badge ' + m.state }, STATE_LABEL[m.state] || m.state), ' ', h('span', { class: 'muted' }, m.mirror)))),
    h('td', { class: 'nowrap' }, ago(r.created_at)),
    h('td', { class: 'actions' },
      h('button', { class: 'secondary small', onclick: action(async () => {
        const adopt = r.mirrors.some((m) => m.state === 'conflict') && confirm('有镜像上已存在同名 Release（不是 Gitrepo 建的）。要接管并覆盖它的标题和说明吗？它原有的附件会保留。');
        const res = await api('POST', path(r) + '/publish', { wait: true, adopt });
        rerender();
        const bad = res.mirrors.filter((m) => m.state === 'failed' || m.state === 'conflict');
        return bad.length ? `${bad.length} 个镜像未发布，失败的会自动重试` : (res.mirrors.some((m) => m.state === 'waiting') ? '已发布；部分镜像等标签同步后再创建' : '已发布到所有镜像');
      }) }, r.published ? '重新发布' : '发布'), ' ',
      h('button', { class: 'danger small', onclick: action(async () => {
        if (!confirm(`删除 Release ${r.tag}？各镜像上由 Gitrepo 创建的 Release 也会被删除（标签保留）。`)) return false;
        await api('DELETE', path(r));
        rerender();
      }, '已删除') }, '删除')))), '还没有 Release。');
}

function fmtSize(n) {
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KiB';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MiB';
  return (n / 1073741824).toFixed(2) + ' GiB';
}

function releaseForm(base, repo) {
  const files = h('input', { type: 'file', name: 'files', multiple: true });
  return h('form', { class: 'grid', onsubmit: action(async (ev) => {
    const d = formData(ev.target);
    if (!d.tag) throw new Error('请填写标签');
    await api('POST', base + '/releases', { tag: d.tag, name: d.name || d.tag, body: d.body, prerelease: d.prerelease, publish: false });
    const path = base + '/releases/' + enc(d.tag);
    for (const f of files.files) {
      toast(`正在上传 ${f.name}…`);
      const res = await fetch(path + '/assets/' + enc(f.name), {
        method: 'PUT', body: f, headers: { Authorization: 'Bearer ' + token, 'Content-Type': f.type || 'application/octet-stream' },
      });
      if (!res.ok) {
        let msg = res.statusText;
        try { msg = (await res.json()).error || msg; } catch (_) { /* not JSON */ }
        throw new Error(`${f.name}：${msg}`);
      }
    }
    if (!d.draft) await api('POST', path + '/publish', { wait: true });
    rerender();
    return d.draft ? '已保存为草稿' : '已发布';
  }) },
  field('标签（需已推到 Hub）', h('input', { name: 'tag', required: true, placeholder: 'v1.2.0' })),
  field('标题', h('input', { name: 'name', placeholder: '默认与标签相同' })),
  field('说明', h('textarea', { name: 'body', placeholder: '更新内容…' }), 'wide'),
  field('附件', files, 'wide'),
  h('div', { class: 'wide row' },
    h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'prerelease' }), '预发布'),
    h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'draft' }), '只保存为草稿，暂不发布')),
  h('div', {}, h('button', { type: 'submit' }, '创建')));
}
