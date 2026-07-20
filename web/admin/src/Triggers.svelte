<script>
  import { getJSON, sendJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';
  let { token } = $props();
  let triggers = $state([]);
  let bindings = $state([]);
  let err = $state('');
  let msg = $state('');
  let loading = $state(false);

  export async function refresh() {
    err = ''; loading = true;
    try {
      triggers = await getJSON(token, '/api/triggers');
      bindings = await getJSON(token, '/api/bindings');
    } catch (e) { err = String(e); }
    loading = false;
  }

  async function act(name, verb, method = 'POST', url) {
    err = ''; msg = '';
    try {
      const j = await sendJSON(token, method, url || ('/api/triggers/' + encodeURIComponent(name) + '/' + verb));
      msg = j.result || (verb + ' ok');
      refresh();
    } catch (e) { err = String(e); }
  }

  function del(name) {
    if (!confirm(t('triggers.confirmremove', { name }))) return;
    act(name, 'delete', 'DELETE', '/api/triggers/' + encodeURIComponent(name));
  }

  let f = $state({ name: '', binding: '', cron: '', timezone: 'Asia/Taipei', message: '', catch_up: true });
  let adding = $state(false);

  async function create() {
    err = ''; msg = '';
    try {
      const j = await sendJSON(token, 'POST', '/api/triggers', {
        name: f.name.trim(),
        binding: f.binding,
        cron: f.cron.trim(),
        timezone: f.timezone.trim() || 'Asia/Taipei',
        message: f.message,
        catch_up: f.catch_up,
      });
      msg = t('triggers.added', { name: j.name || f.name });
      f = { name: '', binding: '', cron: '', timezone: 'Asia/Taipei', message: '', catch_up: true };
      adding = false;
      refresh();
    } catch (e) { err = String(e); }
  }

  $effect(() => { refresh(); });
</script>

<article>
  <header>
    <strong>{t('triggers.title')}</strong> ({triggers.length})
    <button class="mini" onclick={refresh} aria-busy={loading} style="float:right">↻</button>
    <button class="mini secondary" onclick={() => (adding = !adding)} style="float:right; margin-right:.4rem">
      {adding ? t('common.cancel') : t('triggers.add')}
    </button>
  </header>
  {#if err}<p class="bad">{err}</p>{/if}
  {#if msg}<p class="ok">{msg}</p>{/if}

  {#if adding}
    <div class="addform">
      <div class="grid">
        <label>{t('triggers.name')} <input bind:value={f.name} placeholder="daily-report" /></label>
        <label>{t('triggers.binding')}
          <select bind:value={f.binding}>
            <option value="">—</option>
            {#each bindings as b}<option value={b.name}>{b.name}</option>{/each}
          </select>
        </label>
      </div>
      <div class="grid">
        <label>{t('triggers.cron')} <input bind:value={f.cron} placeholder="0 9 * * *" /></label>
        <label>{t('triggers.timezone')} <input bind:value={f.timezone} placeholder="Asia/Taipei" /></label>
      </div>
      <label>{t('triggers.message')} <input bind:value={f.message} placeholder={t('triggers.message.placeholder')} /></label>
      <label class="checkline">
        <input type="checkbox" bind:checked={f.catch_up} />
        {t('triggers.catchup')}
      </label>
      <p class="muted"><small>{t('triggers.cronhint')}</small></p>
      <button onclick={create}>{t('triggers.submit')}</button>
    </div>
  {/if}

  <div style="overflow-x:auto">
    <table>
      <thead>
        <tr>
          <th>{t('triggers.col.name')}</th>
          <th>{t('triggers.col.binding')}</th>
          <th>{t('triggers.col.cron')}</th>
          <th>{t('triggers.col.tz')}</th>
          <th>{t('triggers.col.catchup')}</th>
          <th>{t('triggers.col.status')}</th>
          <th>{t('triggers.col.actions')}</th>
        </tr>
      </thead>
      <tbody>
        {#each triggers as tr}
          <tr>
            <td><strong>{tr.name}</strong></td>
            <td><span class="badge">{tr.binding}</span></td>
            <td><code>{tr.cron}</code></td>
            <td>{tr.timezone || '—'}</td>
            <td>{tr.catch_up ? '✅' : '—'}</td>
            <td>{#if tr.enabled}🟢{:else}<span class="muted">⏸ {t('triggers.disabled')}</span>{/if}</td>
            <td class="actions">
              <button class="mini secondary" title={t('triggers.testhint')} onclick={() => act(tr.name, 'test')}>▶</button>
              {#if tr.enabled}
                <button class="mini secondary" onclick={() => act(tr.name, 'disable')}>⏸</button>
              {:else}
                <button class="mini secondary" onclick={() => act(tr.name, 'enable')}>▶️</button>
              {/if}
              <button class="mini contrast outline" onclick={() => del(tr.name)}>🗑</button>
            </td>
          </tr>
        {/each}
        {#if triggers.length === 0}
          <tr><td colspan="7"><em class="muted">{t('common.none')}</em></td></tr>
        {/if}
      </tbody>
    </table>
  </div>
</article>

<style>
  .badge { font-size: .72rem; padding: .1rem .5rem; border-radius: 1rem; background: var(--pico-secondary-background); color: var(--pico-secondary-inverse); }
  .muted { color: var(--pico-muted-color); }
  .bad { color: var(--pico-del-color); }
  .ok { color: var(--pico-ins-color); }
  table { font-size: .95rem; }
  table td, table th { padding: .6rem .7rem; vertical-align: middle; }
  .actions { white-space: nowrap; }
  .mini { width: auto; padding: .3rem .6rem; font-size: .9rem; margin: 0 .15rem; }
  .addform { border: 1px solid var(--pico-muted-border-color); border-radius: var(--pico-border-radius); padding: .8rem 1rem; margin-bottom: 1rem; }
  .addform label { font-size: .8rem; color: var(--pico-muted-color); }
  .checkline { display: flex; align-items: center; gap: .4rem; font-size: .85rem; }
  .addform button { width: auto; }
</style>
