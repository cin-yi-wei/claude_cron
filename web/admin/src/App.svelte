<script>
  import Chat from './Chat.svelte';
  let token = $state(localStorage.getItem('cc_admin_token') || '');
  let bindings = $state([]);
  let err = $state('');
  let loading = $state(false);
  let open = $state([]); // names of bindings whose chat window is open

  function hdr() {
    return token ? { Authorization: 'Bearer ' + token } : {};
  }

  function active(b) {
    return !b.paused && b._alive;
  }

  function toggleChat(name) {
    open = open.includes(name) ? open.filter((n) => n !== name) : [...open, name];
  }

  async function refresh() {
    err = ''; loading = true;
    localStorage.setItem('cc_admin_token', token);
    try {
      const r = await fetch('/api/bindings', { headers: hdr() });
      if (!r.ok) throw new Error('list failed: ' + r.status);
      const rows = await r.json();
      for (const b of rows) {
        try {
          const st = await (await fetch('/api/bindings/' + encodeURIComponent(b.name), { headers: hdr() })).json();
          b._alive = st.session_alive;
          b._q = 'p' + st.pending + ' / r' + st.processing + ' / f' + st.failed;
        } catch { b._q = '?'; }
      }
      bindings = rows;
      // Drop chats for bindings that vanished.
      const names = new Set(rows.map((b) => b.name));
      open = open.filter((n) => names.has(n));
    } catch (e) { err = String(e); }
    loading = false;
  }

  $effect(() => { refresh(); });
</script>

<main class="container">
  <hgroup>
    <h1>claude_cron admin <small>· svelte</small></h1>
    <p>新版介面 (v2) — bindings + 瀏覽器聊天窗</p>
  </hgroup>

  <article>
    <div class="grid">
      <input type="password" bind:value={token} placeholder="bearer token" autocomplete="off" />
      <div><button onclick={refresh} aria-busy={loading}>Refresh</button></div>
    </div>
    {#if err}<p style="color:var(--pico-del-color)">{err}</p>{/if}
  </article>

  <article>
    <header><strong>Bindings</strong> ({bindings.length})</header>
    <div style="overflow-x:auto">
      <table>
        <thead><tr><th>name</th><th>transport</th><th>plane</th><th>session</th><th>queue</th><th></th></tr></thead>
        <tbody>
          {#each bindings as b}
            <tr>
              <td><strong>{b.name}</strong></td>
              <td><span class="badge">{b.platform} · {b.transport}</span></td>
              <td>{b.plane}</td>
              <td>{#if b.paused}<span class="muted">⏸ paused</span>{:else}{b.tmux_session} {b._alive ? '🟢' : '🔴'}{/if}</td>
              <td>{b._q || ''}</td>
              <td>
                {#if active(b)}
                  <button class="chat-btn {open.includes(b.name) ? '' : 'outline'}" onclick={() => toggleChat(b.name)}>
                    💬 {open.includes(b.name) ? '關閉' : '聊天'}
                  </button>
                {:else}
                  <span class="muted" title="session 未啟動">—</span>
                {/if}
              </td>
            </tr>
          {/each}
          {#if bindings.length === 0}
            <tr><td colspan="6"><em>none (check token)</em></td></tr>
          {/if}
        </tbody>
      </table>
    </div>
  </article>

  {#if open.length > 0}
    <section class="chats">
      {#each open as name (name)}
        <Chat {name} {token} />
      {/each}
    </section>
  {/if}

  <p><small>active session 按「聊天」開窗。settings / create 仍在舊版 / (Pico)，之後搬過來。</small></p>
</main>

<style>
  .badge { font-size: .72rem; padding: .1rem .5rem; border-radius: 1rem; background: var(--pico-secondary-background); color: var(--pico-secondary-inverse); }
  .muted { color: var(--pico-muted-color); }
  table { font-size: .85rem; }
  .chat-btn { width: auto; padding: .15rem .55rem; font-size: .75rem; margin: 0; }
</style>
