<script>
  let token = $state(localStorage.getItem('cc_admin_token') || '');
  let bindings = $state([]);
  let err = $state('');
  let loading = $state(false);

  function hdr() {
    return token ? { Authorization: 'Bearer ' + token } : {};
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
    } catch (e) { err = String(e); }
    loading = false;
  }

  $effect(() => { refresh(); });
</script>

<main class="container">
  <hgroup>
    <h1>claude_cron admin <small>· svelte</small></h1>
    <p>新版介面 (v2) — bindings；之後加聊天窗</p>
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
        <thead><tr><th>name</th><th>transport</th><th>plane</th><th>session</th><th>queue</th></tr></thead>
        <tbody>
          {#each bindings as b}
            <tr>
              <td><strong>{b.name}</strong></td>
              <td><span class="badge">{b.platform} · {b.transport}</span></td>
              <td>{b.plane}</td>
              <td>{#if b.paused}<span class="muted">⏸ paused</span>{:else}{b.tmux_session} {b._alive ? '🟢' : '🔴'}{/if}</td>
              <td>{b._q || ''}</td>
            </tr>
          {/each}
          {#if bindings.length === 0}
            <tr><td colspan="5"><em>none (check token)</em></td></tr>
          {/if}
        </tbody>
      </table>
    </div>
  </article>

  <p><small>完整功能 (settings / create / chat) 會逐步從舊版 / (Pico) 搬過來</small></p>
</main>

<style>
  .badge { font-size: .72rem; padding: .1rem .5rem; border-radius: 1rem; background: var(--pico-secondary-background); color: var(--pico-secondary-inverse); }
  .muted { color: var(--pico-muted-color); }
  table { font-size: .85rem; }
</style>
