<script>
  import Bindings from './Bindings.svelte';
  import Settings from './Settings.svelte';
  import CreateBinding from './CreateBinding.svelte';
  import ChatLayout from './ChatLayout.svelte';
  import { t, getLocale, setLocale, LOCALES } from './lib/i18n.svelte.js';

  let token = $state(localStorage.getItem('cc_admin_token') || '');
  $effect(() => { localStorage.setItem('cc_admin_token', token); });

  // Theme: default to the saved choice, else follow the OS. Applied to <html>
  // via Pico's data-theme.
  const osDark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  let theme = $state(localStorage.getItem('cc_theme') || (osDark ? 'dark' : 'light'));
  $effect(() => {
    document.documentElement.setAttribute('data-theme', theme);
    localStorage.setItem('cc_theme', theme);
  });

  function parseHash() {
    const h = location.hash.replace(/^#\/?/, '');
    const [view, arg] = h.split('/');
    return { view: view || 'bindings', arg: arg ? decodeURIComponent(arg) : '' };
  }
  let route = $state(parseHash());
  $effect(() => {
    const fn = () => (route = parseHash());
    window.addEventListener('hashchange', fn);
    return () => window.removeEventListener('hashchange', fn);
  });

  const nav = [
    { id: 'bindings', key: 'nav.bindings', href: '#/bindings' },
    { id: 'chat', key: 'nav.chat', href: '#/chat' },
    { id: 'create', key: 'nav.create', href: '#/create' },
    { id: 'settings', key: 'nav.settings', href: '#/settings' },
  ];
</script>

<nav class="container-fluid topnav">
  <ul class="navrow">
    <li class="brand"><strong>claude_cron</strong></li>
    {#each nav as n}
      <li><a href={n.href} class={route.view === n.id ? 'active' : ''}>{t(n.key)}</a></li>
    {/each}
    <li class="spacer"></li>
    <li>
      <button class="themebtn" title="theme" onclick={() => (theme = theme === 'dark' ? 'light' : 'dark')}>
        {theme === 'dark' ? '☀️' : '🌙'}
      </button>
    </li>
    <li>
      <select class="lang" value={getLocale()} onchange={(e) => setLocale(e.currentTarget.value)}>
        {#each LOCALES as l}<option value={l.id}>{l.label}</option>{/each}
      </select>
    </li>
  </ul>
</nav>

<main class="container">
  {#if route.view === 'bindings'}
    <Bindings {token} />
  {:else if route.view === 'create'}
    <CreateBinding {token} onCreated={() => (location.hash = '#/bindings')} />
  {:else if route.view === 'settings'}
    <Settings bind:token />
  {:else if route.view === 'chat'}
    <ChatLayout name={route.arg || ''} {token} />
  {/if}
</main>

<style>
  .topnav { padding: .3rem 1rem; border-bottom: 1px solid var(--pico-muted-border-color); position: sticky; top: 0; background: var(--pico-background-color); z-index: 10; }
  /* Single row: brand + nav links + (spacer) + theme/lang. nowrap keeps it on
     one line; it scrolls horizontally only if a very narrow screen can't fit. */
  .navrow { display: flex; align-items: center; flex-wrap: nowrap; gap: .25rem .4rem; margin: 0; list-style: none; padding: 0; overflow-x: auto; }
  .navrow li { flex: 0 0 auto; }
  .navrow .brand { margin-right: .4rem; white-space: nowrap; }
  .navrow .spacer { flex: 1 1 auto; }
  .topnav a { display: block; padding: .35rem .55rem; border-radius: var(--pico-border-radius); text-decoration: none; white-space: nowrap; }
  .topnav a.active { background: var(--pico-primary-background); color: var(--pico-primary-inverse); }
  .muted { color: var(--pico-muted-color); }
  .themebtn { width: auto; padding: .2rem .4rem; margin: 0; background: transparent; border: 1px solid var(--pico-muted-border-color); border-radius: var(--pico-border-radius); line-height: 1; cursor: pointer; }
  .lang { width: auto; font-size: .8rem; padding: .15rem 1.2rem .15rem .35rem; margin: 0; }
  main.container { max-width: 1280px; padding-top: 1.2rem; }

  @media (max-width: 640px) {
    .topnav { padding: .3rem .5rem; }
    .navrow { gap: .15rem .2rem; }
    .navrow .spacer { display: none; }
    .navrow .brand { font-size: .9rem; margin-right: .25rem; }
    .topnav a { padding: .35rem .4rem; font-size: .85rem; }
    .lang { font-size: .75rem; padding: .15rem 1rem .15rem .3rem; }
    main.container { padding-top: .6rem; }
  }
</style>
