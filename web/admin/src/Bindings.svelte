<script>
  import { getJSON, sendJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';
  let { token } = $props();
  let bindings = $state([]);
  let err = $state('');
  let msg = $state('');
  let loading = $state(false);

  function active(b) { return !b.paused && b._alive; }
  function chattable(b) { return active(b) || b.sleeping; } // sending wakes a slept binding

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

  let delTarget = $state(null);   // binding being deleted (opens the modal)
  let delChannel = $state(false); // opt-in: also delete the dc/tg channel

  function del(b) { delTarget = b; delChannel = false; }
  function cancelDel() { delTarget = null; }
  function confirmDel() {
    const name = delTarget.name;
    const wantChannel = delChannel && delTarget.platform === 'discord';
    let url = '/api/bindings/' + encodeURIComponent(name);
    if (wantChannel) url += '?delete_channel=true';
    delTarget = null;
    act(name, 'delete', 'DELETE', url);
  }

  $effect(() => { refresh(); });
</script>

<article>
  <header>
    <strong>{t('bindings.title')}</strong> ({bindings.length})
    <button class="mini" onclick={refresh} aria-busy={loading} style="float:right">↻</button>
  </header>
  {#if err}<p class="bad">{err}</p>{/if}
  {#if msg}<p class="ok">{msg}</p>{/if}
  <div style="overflow-x:auto">
    <table>
      <thead><tr><th>{t('bindings.col.name')}</th><th>{t('bindings.col.kind')}</th><th>{t('bindings.col.session')}</th><th>{t('bindings.col.queue')}</th><th>{t('bindings.col.actions')}</th></tr></thead>
      <tbody>
        {#each bindings as b}
          <tr>
            <td>
              <strong>{b.name}</strong>
              {#if b.control}<span class="badge ctrl">{t('bindings.control')}{b.default ? ' 🛡' : ''}</span>{/if}
            </td>
            <td><span class="badge">{b.platform} · {b.transport}</span></td>
            <td>{#if b.paused}<span class="muted">{t('bindings.paused')}</span>{:else if b.sleeping}<span class="muted">💤 sleeping</span>{:else}{b._alive ? '🟢' : '🔴'} <small class="muted">{b.tmux_session}</small>{/if}</td>
            <td><small>{b._q || ''}</small></td>
            <td class="actions">
              {#if chattable(b)}
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
                <button class="mini contrast outline" onclick={() => del(b)}>🗑</button>
              {/if}
            </td>
          </tr>
        {/each}
        {#if bindings.length === 0}
          <tr><td colspan="5"><em class="muted">{t('common.none')}</em></td></tr>
        {/if}
      </tbody>
    </table>
  </div>
</article>

{#if delTarget}
  <dialog open>
    <article>
      <header><strong>{t('unbind.title', { name: delTarget.name })}</strong></header>
      <p>{t('unbind.confirm', { name: delTarget.name })}</p>
      {#if delTarget.platform === 'discord'}
        <label>
          <input type="checkbox" bind:checked={delChannel} />
          {t('unbind.deletechannel')}
        </label>
      {/if}
      <footer>
        <button class="secondary" onclick={cancelDel}>{t('common.cancel')}</button>
        <button class="contrast" onclick={confirmDel}>{delChannel ? t('unbind.confirm.both') : t('unbind.confirm.keep')}</button>
      </footer>
    </article>
  </dialog>
{/if}

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
