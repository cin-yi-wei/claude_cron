<script>
  let { name, token } = $props();
  let messages = $state([]);
  let input = $state('');
  let status = $state('connecting…');
  let es;

  async function loadHistory() {
    try {
      const headers = token ? { Authorization: 'Bearer ' + token } : {};
      const r = await fetch('/api/chat/' + encodeURIComponent(name) + '/history', { headers });
      if (r.ok) {
        const past = await r.json();
        if (Array.isArray(past)) messages = past;
      }
    } catch {}
  }

  function connect() {
    if (es) es.close();
    status = 'connecting…';
    const q = token ? '?token=' + encodeURIComponent(token) : '';
    es = new EventSource('/api/chat/' + encodeURIComponent(name) + '/stream' + q);
    es.onopen = () => { status = 'live'; };
    es.onerror = () => { status = 'reconnecting…'; };
    es.onmessage = (e) => {
      try {
        const ev = JSON.parse(e.data);
        messages = [...messages, ev];
      } catch {}
    };
  }

  async function send() {
    const text = input.trim();
    if (!text) return;
    input = '';
    const headers = { 'Content-Type': 'application/json' };
    if (token) headers.Authorization = 'Bearer ' + token;
    try {
      const r = await fetch('/api/chat/' + encodeURIComponent(name) + '/send', {
        method: 'POST', headers, body: JSON.stringify({ text })
      });
      if (!r.ok) messages = [...messages, { role: 'error', text: 'send failed: ' + r.status }];
    } catch (e) {
      messages = [...messages, { role: 'error', text: String(e) }];
    }
  }

  $effect(() => {
    // Load the existing thread first so the window isn't blank, then stream live.
    loadHistory().then(connect);
    return () => { if (es) es.close(); };
  });
</script>

<article class="chat">
  <header><strong>💬 {name}</strong> <small>· {status}</small></header>
  <div class="log">
    {#each messages as m}
      <div class="msg {m.role}">
        <span class="who">{m.role === 'assistant' ? '🤖' : m.role === 'user' ? '🧑' : '⚠️'}</span>
        <span class="txt">{m.text}</span>
      </div>
    {/each}
    {#if messages.length === 0}<p class="muted"><em>還沒有訊息。輸入後送出，會注入 cc-{name} session。</em></p>{/if}
  </div>
  <form onsubmit={(e) => { e.preventDefault(); send(); }}>
    <div role="group">
      <input bind:value={input} placeholder="輸入訊息給 {name}…" />
      <button type="submit">送出</button>
    </div>
  </form>
</article>

<style>
  .chat .log { max-height: 320px; overflow-y: auto; display: flex; flex-direction: column; gap: .4rem; padding: .3rem 0; }
  .msg { display: flex; gap: .5rem; font-size: .85rem; }
  .msg .who { flex: 0 0 1.4rem; }
  .msg .txt { white-space: pre-wrap; word-break: break-word; }
  .msg.assistant .txt { color: var(--pico-color); }
  .msg.user .txt { color: var(--pico-primary); }
  .msg.error .txt { color: var(--pico-del-color); }
  .muted { color: var(--pico-muted-color); }
  form { margin: .5rem 0 0; }
</style>
