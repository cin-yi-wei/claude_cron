<script>
  import { getJSON, sendJSON } from './lib/api.js';
  let { token } = $props();
  let bindings = $state([]);
  let err = $state('');
  let msg = $state('');
  let loading = $state(false);

  function active(b) { return !b.paused && b._alive; }

  export async function refresh() {
    err = ''; loading = true;
    try {
      const rows = await getJSON(token, '/api/bindings');
      for (const b of rows) {
        try {
          const st = await getJSON(token, '/api/bindings/' + encodeURIComponent(b.name));
          b._alive = st.session_alive;
          b._q = 'p' + st.pending + ' / r' + st.processing + ' / f' + st.failed;
        } catch { b._q = '?'; }
      }
      bindings = rows;
    } catch (e) { err = String(e); }
    loading = false;
  }

  async function act(name, verb, method = 'POST', url) {
    err = ''; msg = '';
    try {
      const j = await sendJSON(token, method, url || ('/api/bindings/' + encodeURIComponent(name) + '/' + verb));
      msg = j.result || (verb + ' ok');
      refresh();
    } catch (e) { err = String(e); }
  }

  function del(name) {
    if (!confirm('unbind ' + name + '?')) return;
    act(name, 'delete', 'DELETE', '/api/bindings/' + encodeURIComponent(name));
  }

  $effect(() => { refresh(); });
</script>

<article>
  <header>
    <strong>Bindings</strong> ({bindings.length})
    <button class="mini" onclick={refresh} aria-busy={loading} style="float:right">↻</button>
  </header>
  {#if err}<p class="bad">{err}</p>{/if}
  {#if msg}<p class="ok">{msg}</p>{/if}
  <div style="overflow-x:auto">
    <table>
      <thead><tr><th>name</th><th>kind</th><th>session</th><th>queue</th><th>actions</th></tr></thead>
      <tbody>
        {#each bindings as b}
          <tr>
            <td>
              <strong>{b.name}</strong>
              {#if b.control}<span class="badge ctrl">control{b.default ? ' 🛡' : ''}</span>{/if}
            </td>
            <td><span class="badge">{b.platform} · {b.transport}</span></td>
            <td>{#if b.paused}<span class="muted">⏸ paused</span>{:else}{b._alive ? '🟢' : '🔴'} <small class="muted">{b.tmux_session}</small>{/if}</td>
            <td><small>{b._q || ''}</small></td>
            <td class="actions">
              {#if active(b)}
                <a role="button" class="mini" href={'#/chat/' + b.name}>💬</a>
              {/if}
              {#if b.paused}
                <button class="mini secondary" onclick={() => act(b.name, 'resume')}>▶</button>
              {:else if !(b.control && b.default)}
                <button class="mini secondary" onclick={() => act(b.name, 'pause')}>⏸</button>
              {/if}
              {#if active(b)}
                <button class="mini secondary" onclick={() => act(b.name, 'restart')}>⟳</button>
              {/if}
              {#if !(b.control && b.default)}
                <button class="mini contrast outline" onclick={() => del(b.name)}>🗑</button>
              {/if}
            </td>
          </tr>
        {/each}
        {#if bindings.length === 0}
          <tr><td colspan="5"><em class="muted">none (check token)</em></td></tr>
        {/if}
      </tbody>
    </table>
  </div>
</article>

<style>
  .badge { font-size: .72rem; padding: .1rem .5rem; border-radius: 1rem; background: var(--pico-secondary-background); color: var(--pico-secondary-inverse); }
  .badge.ctrl { background: var(--pico-primary); color: var(--pico-primary-inverse); margin-left: .35rem; }
  .muted { color: var(--pico-muted-color); }
  .bad { color: var(--pico-del-color); }
  .ok { color: var(--pico-ins-color); }
  table { font-size: .85rem; }
  .actions { white-space: nowrap; }
  .mini { width: auto; padding: .1rem .45rem; font-size: .8rem; margin: 0 .1rem; }
</style>
