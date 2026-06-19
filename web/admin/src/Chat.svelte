<script>
  import { tick } from 'svelte';
  import { getJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';
  let { name, token } = $props();

  let messages = $state([]); // oldest → newest
  let input = $state('');
  let status = $state('connecting'); // connecting | live | reconnecting
  let hasMore = $state(false);
  let loadingOlder = $state(false);
  let historyLoaded = 0; // count of persisted history messages fetched
  let logEl;
  let es;

  const PAGE = 30;

  function nearBottom() {
    if (!logEl) return true;
    return logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 60;
  }
  async function scrollToBottom() {
    await tick();
    if (logEl) logEl.scrollTop = logEl.scrollHeight;
  }

  // Open on the LATEST page; older messages load on scroll-up.
  async function loadInitial() {
    try {
      const d = await getJSON(token, '/api/chat/' + encodeURIComponent(name) + '/history?limit=' + PAGE);
      messages = Array.isArray(d.messages) ? d.messages : [];
      historyLoaded = messages.length;
      hasMore = !!d.has_more;
      await scrollToBottom();
    } catch {}
  }

  async function loadOlder() {
    if (loadingOlder || !hasMore) return;
    loadingOlder = true;
    const prevH = logEl ? logEl.scrollHeight : 0;
    try {
      const d = await getJSON(token, '/api/chat/' + encodeURIComponent(name) + '/history?limit=' + PAGE + '&before=' + historyLoaded);
      const older = Array.isArray(d.messages) ? d.messages : [];
      if (older.length) {
        messages = [...older, ...messages];
        historyLoaded += older.length;
      }
      hasMore = !!d.has_more;
      await tick();
      if (logEl) logEl.scrollTop += logEl.scrollHeight - prevH; // keep view anchored
    } catch {}
    loadingOlder = false;
  }

  function onScroll() {
    if (logEl && logEl.scrollTop < 40) loadOlder();
  }

  function connect() {
    if (es) es.close();
    status = 'connecting';
    const q = token ? '?token=' + encodeURIComponent(token) : '';
    es = new EventSource('/api/chat/' + encodeURIComponent(name) + '/stream' + q);
    es.onopen = () => { status = 'live'; };
    es.onerror = () => { status = 'reconnecting'; };
    es.onmessage = (e) => {
      try {
        const ev = JSON.parse(e.data);
        const stick = nearBottom();
        messages = [...messages, ev];
        if (stick) scrollToBottom();
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
    loadInitial().then(connect);
    return () => { if (es) es.close(); };
  });
</script>

<article class="chat">
  <header><strong>💬 {name}</strong> <small>· {t('chat.status.' + status)}</small></header>
  <div class="log" bind:this={logEl} onscroll={onScroll}>
    {#if loadingOlder}<p class="muted center"><small>⏳</small></p>{/if}
    {#each messages as m}
      <div class="msg {m.role}">
        <span class="who">{m.role === 'assistant' ? '🤖' : m.role === 'user' ? '🧑' : '⚠️'}</span>
        <span class="txt">{m.text}</span>
      </div>
    {/each}
    {#if messages.length === 0}<p class="muted"><em>{t('chat.empty', { name })}</em></p>{/if}
  </div>
  <form onsubmit={(e) => { e.preventDefault(); send(); }}>
    <div role="group">
      <input bind:value={input} placeholder={t('chat.placeholder', { name })} />
      <button type="submit">{t('chat.send')}</button>
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
  .center { text-align: center; margin: 0; }
</style>
