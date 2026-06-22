<script>
  import { getJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';
  import Chat from './Chat.svelte';
  let { name = '', token } = $props();

  let bindings = $state([]);
  let err = $state('');
  let page = $state(0);
  let query = $state('');
  const pageSize = 6;

  async function load() {
    err = '';
    try {
      bindings = await getJSON(token, '/api/bindings');
    } catch (e) { err = String(e); }
  }
  // Web-control bindings have their own chat; every binding is selectable here.
  $effect(() => { load(); });

  // Filter by name (case-insensitive); searching resets to the first page.
  let filtered = $derived(
    query.trim()
      ? bindings.filter((b) => b.name.toLowerCase().includes(query.trim().toLowerCase()))
      : bindings
  );
  $effect(() => { query; page = 0; });
  let pageCount = $derived(Math.max(1, Math.ceil(filtered.length / pageSize)));
  // Clamp the page if the list shrank.
  $effect(() => { if (page > pageCount - 1) page = pageCount - 1; });
  let shown = $derived(filtered.slice(page * pageSize, page * pageSize + pageSize));
</script>

<div class="chatwrap">
  <section class="pane">
    {#if name}
      {#key name}
        <Chat {name} {token} />
      {/key}
    {:else}
      <article>
        <header><strong>{t('nav.chat')}</strong></header>
        <p class="muted">{t('chat.pick')}</p>
      </article>
    {/if}
  </section>

  <aside class="picker">
    <header class="picker-head">
      <strong>{t('chat.windows')}</strong>
      <button class="mini" onclick={load} title="reload">↻</button>
    </header>
    {#if err}<p class="bad">{err}</p>{/if}
    <input class="search" type="search" bind:value={query} placeholder={t('chat.search')} />
    <ul class="chatlist">
      {#each shown as b}
        <li>
          <a href={'#/chat/' + b.name} class={b.name === name ? 'active' : ''}>
            <span class="nm">{b.name}</span>
            <span class="meta">
              {#if b.control}🛠{/if}
              {#if b.sleeping}💤{:else if b.paused}⏸{/if}
              <small>{b.platform}</small>
            </span>
          </a>
        </li>
      {/each}
      {#if filtered.length === 0}
        <li><em class="muted">{bindings.length === 0 ? t('common.none') : t('chat.nomatch')}</em></li>
      {/if}
    </ul>
    {#if pageCount > 1}
      <nav class="pager">
        <button class="mini" disabled={page === 0} onclick={() => (page = Math.max(0, page - 1))}>‹</button>
        <small>{page + 1}/{pageCount}</small>
        <button class="mini" disabled={page >= pageCount - 1} onclick={() => (page = Math.min(pageCount - 1, page + 1))}>›</button>
      </nav>
    {/if}
  </aside>
</div>

<style>
  .chatwrap { display: flex; gap: 1rem; align-items: flex-start; }
  .pane { flex: 1 1 auto; min-width: 0; }
  /* Picker on the LEFT (order:-1): full chat-height column so the pager pins to
     the bottom (margin-top:auto). Pagination keeps the list short, so the
     overflow:auto scrollbar only appears if 7 rows truly don't fit. */
  /* min-height (not fixed height) so a full page of 6 never gets clipped: short
     content → column is 68vh tall and the pager pins to the bottom; tall content
     → the column grows to fit (no clip, no scrollbar). */
  .picker { order: -1; flex: 0 0 12rem; position: sticky; top: 4rem; font-size: .9rem; display: flex; flex-direction: column; min-height: 68vh; }
  .picker-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: .4rem; }
  .search { margin: 0 0 .5rem; padding: .35rem .55rem; font-size: .85rem; height: auto; }
  /* No scrollbar — pagination handles overflow (more items → next page). */
  .chatlist { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: .25rem; }
  .chatlist a { display: flex; justify-content: space-between; align-items: center; gap: .4rem; padding: .35rem .5rem; border-radius: var(--pico-border-radius); text-decoration: none; border: 1px solid var(--pico-muted-border-color); }
  .chatlist a.active { background: var(--pico-primary-background); color: var(--pico-primary-inverse); border-color: var(--pico-primary); }
  .chatlist .nm { flex: 1 1 auto; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
  .chatlist .meta { display: flex; align-items: center; gap: .3rem; white-space: nowrap; font-size: .8rem; }
  .pager { display: flex; align-items: center; justify-content: center; gap: .6rem; margin-top: auto; padding-top: .6rem; border-top: 1px solid var(--pico-muted-border-color); }
  .mini { width: auto; padding: .15rem .5rem; margin: 0; font-size: .85rem; }
  .muted { color: var(--pico-muted-color); }
  .bad { color: var(--pico-del-color); }

  /* Mobile: picker drops below the conversation, list goes horizontal-scroll. */
  @media (max-width: 720px) {
    .chatwrap { flex-direction: column; }
    .picker { flex: 1 1 auto; width: 100%; position: static; order: -1; height: auto; min-height: 0; }
    .chatlist { flex-direction: row; overflow-x: auto; overflow-y: visible; }
    .chatlist a { white-space: nowrap; }
    .pager { margin-top: .5rem; border-top: none; }
  }
</style>
