package channelagent

// adminIndexHTML is a minimal single-page UI served at /. It calls the same
// JSON API (sending the bearer token typed into the field), lists bindings,
// and offers create / delete / restart. Intentionally dependency-free.
const adminIndexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>claude_cron admin</title>
<style>
  body { font: 14px system-ui, sans-serif; margin: 2rem; color: #1c1c1c; }
  h1 { font-size: 1.2rem; }
  table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
  th, td { border: 1px solid #ddd; padding: 6px 8px; text-align: left; }
  th { background: #f5f5f5; }
  .badge { font-size: 11px; padding: 1px 6px; border-radius: 8px; background: #eee; }
  button { cursor: pointer; }
  input { padding: 4px; margin: 2px; }
  #err { color: #b00; white-space: pre-wrap; }
  .ok { color: #070; }
</style>
</head>
<body>
<h1>claude_cron admin</h1>
<div>
  Token: <input id="token" type="password" placeholder="bearer token (blank if loopback)" size="30">
  <button onclick="refresh()">Refresh</button>
  <span id="msg"></span>
</div>
<div id="err"></div>
<table id="tbl"><thead><tr>
  <th>name</th><th>platform/mode</th><th>channel</th><th>branch</th><th>session</th><th>queue</th><th></th>
</tr></thead><tbody></tbody></table>

<h2 style="font-size:1rem">Settings</h2>
<div id="settings">
  discord transport:
  <select id="s_dc"><option value="gateway">gateway</option><option value="poll">poll</option></select>
  &nbsp; telegram transport:
  <select id="s_tg"><option value="webhook">webhook</option><option value="poll">poll</option></select><br>
  push listen: <input id="s_listen" size="18">
  public_url: <input id="s_url" size="28">
  secret: <input id="s_secret" type="password" size="18" placeholder="(unchanged)">
  tg control chat-id: <input id="s_chat" size="14"><br>
  <button onclick="saveSettings()">Save &amp; Restart serve</button>
  <span id="s_msg"></span>
</div>

<h2 style="font-size:1rem">Create binding</h2>
<div>
  <input id="c_name" placeholder="name">
  <input id="c_dir" placeholder="project dir" size="24">
  <input id="c_branch" placeholder="branch">
  <input id="c_platform" placeholder="platform dc|tg" size="10">
  <input id="c_mode" placeholder="mode poll|push" size="10">
  <input id="c_chat" placeholder="chat-id (tg)" size="10">
  <button onclick="create()">Create</button>
</div>

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
        ? '<button onclick="act(\'' + b.name + '\',\'resume\')">resume</button>'
        : '<button onclick="act(\'' + b.name + '\',\'pause\')">pause</button>';
      var sessCell = b.paused ? '⏸️ paused' : (b.tmux_session + (st.session_alive ? ' ✅' : ' ⛔'));
      tr.innerHTML =
        '<td>' + b.name + '</td>' +
        '<td><span class="badge">' + b.platform + ' · ' + b.transport + '</span> <span class="badge">' + b.plane + '</span></td>' +
        '<td>' + b.channel_id + '</td>' +
        '<td>' + b.branch + '</td>' +
        '<td>' + sessCell + '</td>' +
        '<td>p' + st.pending + ' / r' + st.processing + ' / f' + st.failed + '</td>' +
        '<td>' + toggle + ' ' +
        '<button onclick="del(\'' + b.name + '\')">delete</button></td>';
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
