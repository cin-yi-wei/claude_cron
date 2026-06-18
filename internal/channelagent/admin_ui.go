package channelagent

// adminIndexHTML is the single-page admin UI served at /. It styles semantic
// HTML with Pico.css (classless, ~10KB via CDN — mainstream, no build step, no
// JS framework, so it stays lightweight) and drives the same JSON API with the
// bearer token typed into the field. Pico loads from a CDN; if offline the page
// degrades to unstyled-but-functional HTML.
const adminIndexHTML = `<!doctype html>
<html lang="en" data-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>claude_cron admin</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@picocss/pico@2/css/pico.min.css">
<style>
  :root { --pico-font-size: 92%; --pico-spacing: .8rem; }
  main.container { max-width: 1100px; }
  article { padding: 1rem 1.2rem; margin: 1rem 0; }
  article > header { margin: -1rem -1.2rem 1rem; padding: .6rem 1.2rem; }
  table { font-size: .85rem; margin: 0; }
  th, td { padding: .4rem .5rem; }
  .badge { font-size: .72rem; padding: .1rem .5rem; border-radius: 1rem; background: var(--pico-secondary-background); color: var(--pico-secondary-inverse); margin-right: .25rem; white-space: nowrap; }
  .alive { color: var(--pico-ins-color); font-weight: 600; }
  .dead { color: var(--pico-del-color); font-weight: 600; }
  .paused { color: var(--pico-muted-color); }
  td button, td a[role=button] { width: auto; padding: .15rem .55rem; font-size: .75rem; margin: 0 .15rem 0 0; }
  button { width: auto; }
  #err { color: var(--pico-del-color); white-space: pre-wrap; font-size: .85rem; }
  .ok { color: var(--pico-ins-color); }
  label { font-size: .8rem; color: var(--pico-muted-color); }
  label input, label select { margin-top: .2rem; }
  .row-actions { display: flex; }
</style>
</head>
<body>
<main class="container">
  <hgroup>
    <h1>claude_cron admin</h1>
    <p>bindings · transport · settings</p>
  </hgroup>

  <article>
    <div class="grid">
      <input id="token" type="password" placeholder="bearer token (blank if loopback)" autocomplete="off">
      <div><button onclick="refresh();loadSettings();">Refresh</button> &nbsp;<span id="msg"></span></div>
    </div>
    <div id="err"></div>
  </article>

  <article>
    <header><strong>Bindings</strong></header>
    <div style="overflow-x:auto">
    <table id="tbl"><thead><tr>
      <th>name</th><th>transport</th><th>channel</th><th>branch</th><th>session</th><th>queue</th><th>actions</th>
    </tr></thead><tbody></tbody></table>
    </div>
  </article>

  <article>
    <header><strong>Settings</strong> <small>(saving restarts serve to apply)</small></header>
    <div class="grid">
      <label>discord transport
        <select id="s_dc"><option value="gateway">gateway</option><option value="poll">poll</option></select>
      </label>
      <label>telegram transport
        <select id="s_tg"><option value="webhook">webhook</option></select>
      </label>
    </div>
    <div class="grid">
      <label>push listen <input id="s_listen" placeholder="127.0.0.1:8788"></label>
      <label>push public_url <input id="s_url" placeholder="https://…/tg"></label>
    </div>
    <div class="grid">
      <label>push secret <input id="s_secret" type="password" placeholder="(unchanged)" autocomplete="off"></label>
      <label>tg control chat-id <input id="s_chat"></label>
    </div>
    <div class="grid">
      <label>discord bot token <input id="s_dctok" type="password" placeholder="(unchanged)" autocomplete="off"></label>
      <label>telegram bot token <input id="s_tgtok" type="password" placeholder="(unchanged)" autocomplete="off"></label>
    </div>
    <button onclick="saveSettings()">Save &amp; Restart serve</button> &nbsp;<span id="s_msg"></span>
  </article>

  <article>
    <header><strong>Create binding</strong></header>
    <div class="grid">
      <input id="c_name" placeholder="name">
      <input id="c_dir" placeholder="project dir">
      <input id="c_branch" placeholder="branch">
    </div>
    <div class="grid">
      <input id="c_platform" placeholder="platform dc|tg">
      <input id="c_mode" placeholder="mode poll|push">
      <input id="c_chat" placeholder="chat-id (tg)">
    </div>
    <button onclick="create()">Create</button>
  </article>
</main>

<script>
function tok() { return document.getElementById('token').value.trim(); }
function hdr() { var h = {}; var t = tok(); if (t) h['Authorization'] = 'Bearer ' + t; return h; }
function setErr(e) { document.getElementById('err').textContent = e || ''; }
function setMsg(m, ok) { var s = document.getElementById('msg'); s.textContent = m || ''; s.className = ok ? 'ok' : ''; }

async function refresh() {
  setErr('');
  try {
    var r = await fetch('/api/bindings', { headers: hdr() });
    if (!r.ok) throw new Error('list failed: ' + r.status);
    var rows = await r.json();
    var body = document.querySelector('#tbl tbody');
    body.innerHTML = '';
    for (const b of rows) {
      var st = await (await fetch('/api/bindings/' + encodeURIComponent(b.name), { headers: hdr() })).json();
      var tr = document.createElement('tr');
      var toggle = b.paused
        ? '<button class="secondary" onclick="act(\'' + b.name + '\',\'resume\')">resume</button>'
        : '<button class="secondary" onclick="act(\'' + b.name + '\',\'pause\')">pause</button>';
      var sessCell = b.paused
        ? '<span class="paused">⏸ paused</span>'
        : (b.tmux_session + (st.session_alive ? ' <span class="alive">●</span>' : ' <span class="dead">●</span>'));
      tr.innerHTML =
        '<td><strong>' + b.name + '</strong></td>' +
        '<td><span class="badge">' + b.platform + ' · ' + b.transport + '</span><span class="badge">' + b.plane + '</span></td>' +
        '<td>' + b.channel_id + '</td>' +
        '<td>' + b.branch + '</td>' +
        '<td>' + sessCell + '</td>' +
        '<td>p' + st.pending + ' / r' + st.processing + ' / f' + st.failed + '</td>' +
        '<td><div class="row-actions">' + toggle +
        '<button class="contrast outline" onclick="del(\'' + b.name + '\')">delete</button></div></td>';
      body.appendChild(tr);
    }
  } catch (e) { setErr(String(e)); }
}
async function create() {
  setErr(''); setMsg('');
  var payload = {
    name: c_name.value.trim(), project_dir: c_dir.value.trim(), branch: c_branch.value.trim(),
    platform: c_platform.value.trim(), mode: c_mode.value.trim(), chat_id: c_chat.value.trim()
  };
  try {
    var r = await fetch('/api/bindings', { method: 'POST', headers: Object.assign({'Content-Type':'application/json'}, hdr()), body: JSON.stringify(payload) });
    var j = await r.json();
    if (!r.ok) throw new Error(j.error || ('status ' + r.status));
    setMsg(j.result || 'created', true); refresh();
  } catch (e) { setErr(String(e)); }
}
async function del(name) {
  if (!confirm('unbind ' + name + '?')) return;
  setErr('');
  try {
    var r = await fetch('/api/bindings/' + encodeURIComponent(name), { method: 'DELETE', headers: hdr() });
    var j = await r.json();
    if (!r.ok) throw new Error(j.error || ('status ' + r.status));
    setMsg(j.result || 'deleted', true); refresh();
  } catch (e) { setErr(String(e)); }
}
async function act(name, action) {
  setErr('');
  try {
    var r = await fetch('/api/bindings/' + encodeURIComponent(name) + '/' + action, { method: 'POST', headers: hdr() });
    var j = await r.json();
    if (!r.ok) throw new Error(j.error || ('status ' + r.status));
    setMsg(j.result || (action + ' ok'), true); refresh();
  } catch (e) { setErr(String(e)); }
}
async function loadSettings() {
  try {
    var c = await (await fetch('/api/config', { headers: hdr() })).json();
    document.getElementById('s_dc').value = c.discord_transport;
    document.getElementById('s_tg').value = c.telegram_transport;
    document.getElementById('s_listen').value = c.push_listen || '';
    document.getElementById('s_url').value = c.push_public_url || '';
    document.getElementById('s_secret').placeholder = c.push_secret_set ? '(set — blank=keep)' : '(none)';
    document.getElementById('s_chat').value = c.telegram_chat_id || '';
    document.getElementById('s_dctok').placeholder = c.discord_token_set ? '(set — blank=keep)' : '(none)';
    document.getElementById('s_tgtok').placeholder = c.telegram_token_set ? '(set — blank=keep)' : '(none)';
  } catch (e) { /* settings optional; ignore */ }
}
async function saveSettings() {
  setErr('');
  var body = {
    discord_transport: document.getElementById('s_dc').value,
    telegram_transport: document.getElementById('s_tg').value,
    push_listen: document.getElementById('s_listen').value,
    push_public_url: document.getElementById('s_url').value,
    telegram_chat_id: document.getElementById('s_chat').value
  };
  var sec = document.getElementById('s_secret').value;
  if (sec) body.push_secret = sec;
  var dctok = document.getElementById('s_dctok').value;
  if (dctok) body.discord_token = dctok;
  var tgtok = document.getElementById('s_tgtok').value;
  if (tgtok) body.telegram_token = tgtok;
  if (!confirm('Save settings and restart serve?')) return;
  try {
    var r = await fetch('/api/config', { method: 'PUT', headers: Object.assign({'Content-Type':'application/json'}, hdr()), body: JSON.stringify(body) });
    var j = await r.json();
    if (!r.ok) throw new Error(j.error || ('status ' + r.status));
    setMsg(j.restarting ? 'saved — restarting serve…' : 'saved (restart manually)', true);
  } catch (e) { setErr(String(e)); }
}
refresh();
loadSettings();
</script>
</body>
</html>`
