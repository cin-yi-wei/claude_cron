<script>
  import { tick } from 'svelte';
  import { getJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';
  // Syntax highlighting: hljs core + only the languages we care about (keeps the
  // bundle small vs the full build). diff blocks keep our own red/green render.
  import hljs from 'highlight.js/lib/core';
  import go from 'highlight.js/lib/languages/go';
  import javascript from 'highlight.js/lib/languages/javascript';
  import typescript from 'highlight.js/lib/languages/typescript';
  import python from 'highlight.js/lib/languages/python';
  import bash from 'highlight.js/lib/languages/bash';
  import json from 'highlight.js/lib/languages/json';
  import ruby from 'highlight.js/lib/languages/ruby';
  import 'highlight.js/styles/github-dark.css';
  hljs.registerLanguage('go', go);
  hljs.registerLanguage('javascript', javascript);
  hljs.registerLanguage('typescript', typescript);
  hljs.registerLanguage('python', python);
  hljs.registerLanguage('bash', bash);
  hljs.registerLanguage('json', json);
  hljs.registerLanguage('ruby', ruby);

  function hl(lang, code) {
    if (lang && hljs.getLanguage(lang)) {
      try { return hljs.highlight(code, { language: lang, ignoreIllegals: true }).value; } catch {}
    }
    return code.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

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
    // SSE only pushes to currently-connected clients — messages that arrive while
    // the page is backgrounded (e.g. phone screen off, browser freezes the
    // connection) are missed. On resume, re-fetch the latest history (persisted,
    // so it includes anything missed) and reconnect the stream.
    const onVis = () => {
      if (document.visibilityState !== 'visible') return;
      loadInitial();
      connect();
    };
    document.addEventListener('visibilitychange', onVis);
    return () => {
      document.removeEventListener('visibilitychange', onVis);
      if (es) es.close();
    };
  });

  // Split a message into plain-text and fenced-code segments so ```diff blocks
  // render with red/green line colouring (matching Discord).
  function segments(text) {
    const parts = [];
    const re = /```(\w*)\n?([\s\S]*?)```/g;
    let last = 0, mm;
    while ((mm = re.exec(text))) {
      if (mm.index > last) parts.push({ t: 'text', c: text.slice(last, mm.index) });
      parts.push({ t: mm[1] === 'diff' ? 'diff' : 'code', lang: mm[1], c: mm[2].replace(/\n$/, '') });
      last = re.lastIndex;
    }
    if (last < text.length) parts.push({ t: 'text', c: text.slice(last) });
    return parts;
  }
  function dcls(line) {
    if (line[0] === '+') return 'add';
    if (line[0] === '-') return 'del';
    return '';
  }
</script>

<article class="chat">
  <header><strong>💬 {name}</strong> <small>· {t('chat.status.' + status)}</small></header>
  <div class="log" bind:this={logEl} onscroll={onScroll}>
    {#if loadingOlder}<p class="muted center"><small>⏳</small></p>{/if}
    {#each messages as m}
      <div class="msg {m.role}">
        <span class="who">{m.role === 'assistant' ? '🤖' : m.role === 'user' ? '🧑' : '⚠️'}</span>
        <span class="txt">
          {#each segments(m.text) as seg}
            {#if seg.t === 'diff'}
              <pre class="code diff">{#each seg.c.split('\n') as ln}<span style={dcls(ln) === 'add' ? 'color:#3fb950' : dcls(ln) === 'del' ? 'color:#f85149' : ''}>{ln}{'\n'}</span>{/each}</pre>
            {:else if seg.t === 'code'}
              <pre class="code hljs"><code>{@html hl(seg.lang, seg.c)}</code></pre>
            {:else}<span>{seg.c}</span>{/if}
          {/each}
        </span>
      </div>
    {/each}
    {#if messages.length === 0}<p class="muted"><em>{t('chat.empty', { name })}</em></p>{/if}
  </div>
  <form onsubmit={(e) => { e.preventDefault(); send(); }}>
    <div role="group" class="composer">
      <textarea
        bind:value={input}
        rows="2"
        placeholder={t('chat.placeholder', { name })}
        onkeydown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
            e.preventDefault();
            send();
          }
        }}
      ></textarea>
      <button type="submit">{t('chat.send')}</button>
    </div>
  </form>
</article>

<style>
  /* Fill most of the viewport instead of a tiny fixed 320px box. */
  .chat .log { height: 68vh; min-height: 360px; overflow-y: auto; display: flex; flex-direction: column; gap: .55rem; padding: .5rem .3rem; }
  /* Bubble layout: each message gets its own tinted box. User (我方) goes to the
     right, Claude (assistant) to the left, like a normal IM. */
  .msg { display: flex; gap: .5rem; font-size: .98rem; line-height: 1.5; align-items: flex-end; }
  .msg.user { flex-direction: row-reverse; }
  .msg .who { flex: 0 0 1.8rem; text-align: center; }
  .msg .txt { white-space: pre-wrap; word-break: break-word; max-width: 82%; padding: .5rem .75rem; border-radius: 14px; border: 1px solid var(--pico-muted-border-color); background: var(--pico-card-background-color, #21232e); }
  .msg .txt .code { white-space: pre; overflow-x: auto; margin: .3rem 0; padding: .5rem .7rem; border-radius: 6px; background: var(--pico-code-background-color, #1e2030); font-size: .9rem; line-height: 1.45; }
  /* diff line colours are applied inline (Svelte can't keep CSS for runtime-only classes). */
  .msg.assistant .txt { border-bottom-left-radius: 4px; }
  .msg.user .txt { background: var(--pico-primary-background); color: var(--pico-primary-inverse); border-color: transparent; border-bottom-right-radius: 4px; }
  .msg.error .txt { background: var(--pico-del-color); color: #fff; border-color: transparent; }
  .muted { color: var(--pico-muted-color); }
  .center { text-align: center; margin: 0; }
  /* Composer: multi-line textarea (Enter sends, Shift+Enter newline), send
     button aligned to the bottom so it doesn't stretch with the textarea. */
  .composer { align-items: stretch; }
  .composer textarea { resize: vertical; min-height: 2.6rem; max-height: 40vh; line-height: 1.45; margin: 0; }
  .composer button { white-space: nowrap; align-self: stretch; }
</style>
