// taihu 集群管理页前端：单页 + fetch 调 /api/*，概览轮询刷新，危险操作二次确认。
'use strict';

const el = id => document.getElementById(id);

async function api(path, opts) {
  const resp = await fetch(path, opts);
  if (!resp.ok) {
    let msg = 'HTTP ' + resp.status;
    try { const j = await resp.json(); if (j.error) msg = j.error; } catch (_) {}
    throw new Error(msg);
  }
  return resp.json();
}

function showErr(id, e) {
  const n = el(id);
  n.innerHTML = `<div class="errbox">${e.message}</div>`;
}

function fmtBytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(n >= 10 || i === 0 ? 0 : 1) + ' ' + u[i];
}

// ---------- 概览（cluster status 轮询） ----------

function card(s, l) {
  const badge = s.error
    ? `<span class="badge stale">${s.error}</span>`
    : `<span class="badge online">online</span>`;
  let bar = '';
  if (l && l.capacity > 0) {
    const pct = Math.round(l.used / l.capacity * 100);
    const cls = pct >= 90 ? 'full' : pct >= 70 ? 'hot' : '';
    bar = `<div class="bar"><div class="${cls}" style="width:${pct}%"></div></div>
      <div class="kv"><span>已用 / 容量</span><b>${fmtBytes(l.used)} / ${fmtBytes(l.capacity)}（${pct}%）</b></div>`;
  }
  return `<div class="card">
    <h3>${s.name} ${badge}</h3>
    <div class="kv"><span>地址</span><b>${s.addr || l.addr || '-'}</b></div>
    <div class="kv"><span>TCP RTT</span><b>${s.tcp_rtt || '-'}</b></div>
    <div class="kv"><span>shm</span><b>${s.shm_ok || '-'}</b></div>
    ${bar}
    <div class="kv"><span>段 free/active/full</span><b>${s.seg_free}/${s.seg_active}/${s.seg_full}</b></div>
    <div class="kv"><span>cursor</span><b>seg ${s.cursor_seg} + ${fmtBytes(s.cursor_off)}</b></div>
    <div class="kv"><span>对象数</span><b>${s.object_count}</b></div>
  </div>`;
}

function renderOverview() {
  Promise.all([api('/api/cluster/status'), api('/api/cluster/list')])
    .then(([status, list]) => {
      el('ov-err').classList.add('hidden');
      const byName = {};
      list.forEach(r => { byName[r.name] = r; });
      const cards = status.map(s => card(s, byName[s.name])).join('');
      el('ov-cards').innerHTML = cards || '<div class="hint">暂无注册实例</div>';
      el('ov-ts').textContent = new Date().toLocaleTimeString();
    })
    .catch(e => {
      el('ov-err').classList.remove('hidden');
      el('ov-err').textContent = '概览加载失败：' + e.message;
    });
}

// ---------- 集群 ----------

function renderClusterList() {
  api('/api/cluster/list').then(rows => {
    if (!rows.length) { el('cl-list').textContent = '暂无注册实例'; return; }
    el('cl-list').innerHTML = `<table><thead><tr>
      <th>name</th><th>node</th><th>addr</th><th>shm</th><th>status</th>
      <th class="num">capacity</th><th class="num">used</th><th class="num">available</th><th>heartbeat</th>
    </tr></thead><tbody>${rows.map(r => `<tr>
      <td>${r.name}</td><td>${r.node}</td><td>${r.addr}</td><td>${r.shm_addr || '-'}</td>
      <td><span class="badge ${r.status}">${r.status}</span></td>
      <td class="num">${fmtBytes(r.capacity)}</td><td class="num">${fmtBytes(r.used)}</td><td class="num">${fmtBytes(r.available)}</td>
      <td>${r.heartbeat_age || '-'}</td>
    </tr>`).join('')}</tbody></table>`;
  }).catch(e => showErr('cl-list', e));
}

function renderIndex() {
  const prefix = el('cl-index-prefix').value.trim();
  const q = prefix ? '?prefix=' + encodeURIComponent(prefix) : '';
  api('/api/cluster/index' + q).then(res => {
    let h = `<div class="hint">匹配索引条目：${res.total}</div>`;
    if (res.instances.length) {
      h += `<table><thead><tr><th>instance</th><th class="num">count</th></tr></thead><tbody>` +
        res.instances.map(i => `<tr><td>${i.name}</td><td class="num">${i.count}</td></tr>`).join('') + '</tbody></table>';
    }
    el('cl-index').innerHTML = h;
  }).catch(e => showErr('cl-index', e));
}

function purgePreview() {
  api('/api/cluster/purge', { method: 'POST' }).then(res => {
    el('cl-purge-res').textContent =
      `预览：实例 ${res.counts.instances}、索引 ${res.counts.index}、客户端 ${res.counts.clients}，共 ${res.total} 条元数据。`;
  }).catch(e => { el('cl-purge-res').textContent = '失败：' + e.message; });
}

function purgeDo() {
  api('/api/cluster/purge?confirm=true', { method: 'POST' }).then(res => {
    el('cl-purge-res').textContent = `已清空 /taihu/ 命名空间 ${res.deleted} 条元数据。`;
    el('cl-purge-confirm').value = '';
    el('cl-purge-btn').disabled = true;
    renderClusterList();
  }).catch(e => { el('cl-purge-res').textContent = '失败：' + e.message; });
}

// ---------- 实例 ----------

function loadInstanceOptions() {
  api('/api/cluster/list').then(rows => {
    const sel = el('in-instance');
    sel.innerHTML = '<option value="">全部在线实例</option>' +
      rows.map(r => `<option>${r.name}</option>`).join('');
    el('in-note').textContent = `${rows.length} 个已注册实例`;
  }).catch(e => { el('in-note').textContent = '加载实例列表失败：' + e.message; });
}

function renderSegments() {
  const inst = el('in-instance').value;
  const detail = el('in-detail').checked;
  const p = new URLSearchParams();
  if (inst) p.set('instance', inst);
  if (detail) p.set('detail', '1');
  api('/api/instance/segments?' + p.toString()).then(rows => {
    if (!rows.length) { el('in-res').textContent = '无实例可查'; return; }
    el('in-res').innerHTML = rows.map(r => {
      let h = `<h3>${r.name}（${r.addr}）</h3>`;
      if (r.error) return `<div class="panel">${h}<div class="errbox">${r.error}</div></div>`;
      h += `<table><tbody>
        <tr><th>total</th><td class="num">${r.summary.total}</td><th>free</th><td class="num">${r.summary.free}</td>
            <th>active</th><td class="num">${r.summary.active}</td><th>full</th><td class="num">${r.summary.full}</td></tr>
        <tr><th>reclaiming</th><td class="num">${r.summary.reclaiming}</td><th>seg size</th><td class="num">${fmtBytes(r.summary.seg_size)}</td>
            <th>cursor</th><td class="num">seg ${r.summary.cursor_seg} + ${fmtBytes(r.summary.cursor_off)}</td>
            <th>objects</th><td class="num">${r.summary.object_count}</td></tr>
      </tbody></table>`;
      if (detail && r.segments && r.segments.length) {
        h += `<table><thead><tr><th class="num">segID</th><th>state</th><th class="num">alive</th><th class="num">reclaim_seq</th></tr></thead><tbody>` +
          r.segments.map(s => `<tr><td class="num">${s.segment_id}</td><td>${s.state}</td><td class="num">${s.alive_count}</td><td class="num">${s.reclaim_seq}</td></tr>`).join('') +
          '</tbody></table>';
      }
      return `<div class="panel">${h}</div>`;
    }).join('');
  }).catch(e => showErr('in-res', e));
}

// ---------- Key ----------

function keyModeChange() {
  const mode = el('key-mode').value;
  el('key-addr').classList.toggle('hidden', mode !== 'addr');
  el('key-inst').classList.toggle('hidden', mode !== 'inst');
}

function keyTargetQuery() {
  const mode = el('key-mode').value;
  const p = new URLSearchParams();
  if (mode === 'addr') { const a = el('key-addr').value.trim(); if (a) p.set('addr', a); }
  if (mode === 'inst') { const i = el('key-inst').value.trim(); if (i) p.set('instance', i); }
  return p.toString();
}

function loadInstDatalist() {
  api('/api/cluster/list').then(rows => {
    let dl = document.getElementById('inst-dl');
    if (!dl) { dl = document.createElement('datalist'); dl.id = 'inst-dl'; document.body.appendChild(dl); }
    dl.innerHTML = rows.map(r => `<option value="${r.name}">`).join('');
    el('key-inst').setAttribute('list', 'inst-dl');
  }).catch(() => {});
}

function keyPut() {
  const file = el('k-put-file').files[0];
  const key = el('k-put-key').value.trim();
  if (!file) { el('k-put-res').textContent = '请选择文件'; return; }
  if (!key) { el('k-put-res').textContent = '请输入 key'; return; }
  const fd = new FormData();
  fd.append('key', key);
  fd.append('file', file);
  const size = el('k-put-size').value.trim();
  if (size) fd.append('size', size);
  const q = keyTargetQuery();
  api('/api/key/put' + (q ? '?' + q : ''), { method: 'POST', body: fd })
    .then(res => { el('k-put-res').textContent = `put "${res.key}" ok（${res.size} 字节）-> ${res.instance}`; })
    .catch(e => { el('k-put-res').textContent = '失败：' + e.message; });
}

function keyGet() {
  const key = el('k-get-key').value.trim();
  if (!key) { el('k-get-res').textContent = '请输入 key'; return; }
  const p = new URLSearchParams();
  p.set('key', key);
  const off = el('k-get-off').value.trim(); if (off) p.set('off', off);
  const size = el('k-get-size').value.trim(); if (size) p.set('size', size);
  const q = keyTargetQuery();
  fetch('/api/key/get?' + p.toString() + (q ? '&' + q : '')).then(resp => {
    if (!resp.ok) { return resp.json().then(j => { throw new Error(j.error || 'HTTP ' + resp.status); }); }
    return resp.blob().then(blob => {
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = key.split('/').pop() || key;
      document.body.appendChild(a); a.click(); a.remove();
      el('k-get-res').textContent = `已下载 ${blob.size} 字节（${key}）`;
    });
  }).catch(e => { el('k-get-res').textContent = '失败：' + e.message; });
}

function keyStat() {
  const key = el('k-stat-key').value.trim();
  if (!key) { el('k-stat-res').textContent = '请输入 key'; return; }
  const q = keyTargetQuery();
  api('/api/key/stat?key=' + encodeURIComponent(key) + (q ? '&' + q : ''))
    .then(res => { el('k-stat-res').textContent = `stat "${res.key}": size=${res.size}（${fmtBytes(res.size)}）-> ${res.instance}`; })
    .catch(e => { el('k-stat-res').textContent = '失败：' + e.message; });
}

function keyMeta() {
  const key = el('k-meta-key').value.trim();
  if (!key) { el('k-meta-res').textContent = '请输入 key'; return; }
  const q = keyTargetQuery();
  api('/api/key/meta?key=' + encodeURIComponent(key) + (q ? '&' + q : ''))
    .then(res => { el('k-meta-res').textContent = `meta "${res.key}": size=${res.size} seg=${res.segment_id} off=${res.offset}`; })
    .catch(e => { el('k-meta-res').textContent = '失败：' + e.message; });
}

function keyList() {
  const p = new URLSearchParams();
  const prefix = el('k-list-prefix').value.trim(); if (prefix) p.set('prefix', prefix);
  const limit = el('k-list-limit').value.trim(); if (limit) p.set('limit', limit);
  const q = keyTargetQuery();
  api('/api/key/list?' + p.toString() + (q ? '&' + q : ''))
    .then(res => { el('k-list-res').textContent = `共 ${res.total} 个 key（显示 ${res.keys.length}）：\n` + res.keys.join('\n'); })
    .catch(e => { el('k-list-res').textContent = '失败：' + e.message; });
}

function delCheck() {
  const k = el('k-del-key').value.trim();
  const c = el('k-del-confirm').value.trim();
  el('k-del-btn').disabled = !(k && k === c);
}

function keyDelete() {
  const key = el('k-del-key').value.trim();
  const q = keyTargetQuery();
  api('/api/key/delete?key=' + encodeURIComponent(key) + '&confirm=true' + (q ? '&' + q : ''), { method: 'DELETE' })
    .then(res => {
      el('k-del-res').textContent = `delete "${res.key}" ok -> ${res.instance}`;
      el('k-del-key').value = ''; el('k-del-confirm').value = ''; delCheck();
    })
    .catch(e => { el('k-del-res').textContent = '失败：' + e.message; });
}

// ---------- 客户端 / 版本 ----------

function renderClients() {
  api('/api/client/list').then(res => {
    if (!res.rows.length) { el('clients-list').textContent = '暂无注册客户端'; return; }
    el('clients-list').innerHTML = `<table><thead><tr>
      <th>ID</th><th>NODE</th><th>HOST</th><th class="num">PID</th><th>SDK_VER</th><th>STATUS</th><th>HEARTBEAT</th>
    </tr></thead><tbody>` + res.rows.map(c => `<tr>
      <td>${c.id}</td><td>${c.node}</td><td>${c.host || '-'}</td><td class="num">${c.pid}</td>
      <td>${c.sdk_version}</td><td><span class="badge ${c.status}">${c.status}</span></td><td>${c.heartbeat_age || '-'}</td>
    </tr>`).join('') + `</tbody></table>
    <div class="hint">共 ${res.total}，在线 ${res.online}，离线 ${res.stale}</div>`;
  }).catch(e => showErr('clients-list', e));
}

function renderClientsInfo() {
  api('/api/client/info').then(info => {
    let h = `<div class="kv"><span>版本</span><b>${info.version}</b></div>
      <div class="kv"><span>go</span><b>${info.go_version}</b></div>
      <div class="kv"><span>pd</span><b>${info.config.pd}</b></div>
      <div class="kv"><span>kv</span><b>${info.kv_status}</b></div>`;
    if (info.kv_counts) {
      h += `<div class="kv"><span>计数</span><b>instances=${info.kv_counts.instances ?? '-'} clients=${info.kv_counts.clients ?? '-'} index=${info.kv_counts.index_entries ?? '-'}</b></div>`;
    }
    if (info.connectivity && info.connectivity.length) {
      h += `<table><thead><tr><th>instance</th><th>addr</th><th>shm</th><th>rtt</th><th>local</th><th>error</th></tr></thead><tbody>` +
        info.connectivity.map(c => `<tr><td>${c.name}</td><td>${c.addr}</td><td>${c.shm}</td><td>${c.rtt || '-'}</td><td>${c.local ? 'LOCAL' : ''}</td><td>${c.error || ''}</td></tr>`).join('') +
        '</tbody></table>';
    }
    el('clients-info').innerHTML = h;
  }).catch(e => showErr('clients-info', e));
}

function renderVersion() {
  api('/api/version').then(v => {
    el('ver-info').innerHTML = `<div class="kv"><span>taihu</span><b>${v.version}</b></div>
      <div class="kv"><span>go</span><b>${v.go_version}</b></div>`;
    el('foot').textContent = `taihu ${v.version}`;
  }).catch(e => showErr('ver-info', e));
}

// ---------- 装配 ----------

function refresh(tab) {
  if (tab === 'overview') renderOverview();
  if (tab === 'cluster') { renderClusterList(); renderIndex(); }
  if (tab === 'instance') loadInstanceOptions();
  if (tab === 'client') renderClients();
  if (tab === 'version') renderVersion();
}

function init() {
  document.querySelectorAll('.tab-btn').forEach(b => {
    b.onclick = () => {
      document.querySelectorAll('.tab-btn').forEach(x => x.classList.remove('active'));
      document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
      b.classList.add('active');
      document.getElementById('tab-' + b.dataset.tab).classList.add('active');
      refresh(b.dataset.tab);
    };
  });

  keyModeChange();
  el('key-mode').onchange = keyModeChange;

  el('ov-refresh').onclick = renderOverview;
  el('cl-list-refresh').onclick = renderClusterList;
  el('cl-index-refresh').onclick = renderIndex;
  el('cl-index-prefix').addEventListener('keydown', e => { if (e.key === 'Enter') renderIndex(); });
  el('cl-purge-preview').onclick = purgePreview;
  el('cl-purge-btn').onclick = purgeDo;
  el('cl-purge-confirm').oninput = () => {
    el('cl-purge-btn').disabled = el('cl-purge-confirm').value.trim() !== 'PURGE';
  };

  el('in-query').onclick = renderSegments;
  el('key-op-reload-inst').onclick = loadInstDatalist;
  el('k-put-btn').onclick = keyPut;
  el('k-get-btn').onclick = keyGet;
  el('k-stat-btn').onclick = keyStat;
  el('k-meta-btn').onclick = keyMeta;
  el('k-list-btn').onclick = keyList;
  el('k-del-key').oninput = delCheck;
  el('k-del-confirm').oninput = delCheck;
  el('k-del-btn').onclick = keyDelete;

  el('clients-refresh').onclick = renderClients;
  el('clients-info-btn').onclick = renderClientsInfo;

  renderVersion();
  renderOverview();
  // 概览每 5s 轮询（仅在概览 tab 激活时）。
  setInterval(() => {
    const active = document.querySelector('.tab-btn.active');
    if (active && active.dataset.tab === 'overview') renderOverview();
  }, 5000);
}

document.addEventListener('DOMContentLoaded', init);
