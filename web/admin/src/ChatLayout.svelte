<script>
  import { getJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';
  import Chat from './Chat.svelte';
  let { name = '', token } = $props();

  let bindings = $state([]);
  let err = $state('');
  let page = $state(0);
  const pageSize = 7;

  async function load() {
    err = '';
    try {
      bindings = await getJSON(token, '/api/bindings');
    } catch (e) { err = String(e); }
  }
  // Web-control bindings have their own chat; every binding is selectable here.
  $effect(() => { load(); });

  let pageCount = $derived(Math.max(1, Math.ceil(bindings.length / pageSize)));
  // Clamp the page if the list shrank.
  $effect(() => { if (page > pageCount - 1) page = pageCount - 1; });
  let shown = $derived(bindings.slice(page * pageSize, page * pageSize + pageSize));
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
      {#if bindings.length === 0}
        <li><em class="muted">{t('common.none')}</em></li>
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
  .picker { flex: 0 0 12rem; position: sticky; top: 4rem; font-size: .9rem; }
  .picker-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: .4rem; }
  .chatlist { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: .25rem; }
  .chatlist a { display: flex; justify-content: space-between; align-items: center; gap: .4rem; padding: .35rem .5rem; border-radius: var(--pico-border-radius); text-decoration: none; border: 1px solid var(--pico-muted-border-color); }
  .chatlist a.active { background: var(--pico-primary-background); color: var(--pico-primary-inverse); border-color: var(--pico-primary); }
  .chatlist .nm { font-weight: 600; word-break: break-all; }
  .chatlist .meta { display: flex; align-items: center; gap: .3rem; white-space: nowrap; font-size: .8rem; }
  .pager { display: flex; align-items: center; justify-content: center; gap: .6rem; margin-top: .5rem; }
  .mini { width: auto; padding: .15rem .5rem; margin: 0; font-size: .85rem; }
  .muted { color: var(--pico-muted-color); }
  .bad { color: var(--pico-del-color); }

  /* Mobile: picker drops below the conversation, list goes horizontal-scroll. */
  @media (max-width: 720px) {
    .chatwrap { flex-direction: column; }
    .picker { flex: 1 1 auto; width: 100%; position: static; order: -1; }
    .chatlist { flex-direction: row; overflow-x: auto; }
    .chatlist a { white-space: nowrap; }
  }
</style>
