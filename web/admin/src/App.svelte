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
  <ul class="brand">
    <li><strong>claude_cron</strong> <small class="muted">admin</small></li>
  </ul>
  <ul class="links">
    {#each nav as n}
      <li><a href={n.href} class={route.view === n.id ? 'active' : ''}>{t(n.key)}</a></li>
    {/each}
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
    <li class="tok-li"><input class="tok" type="password" bind:value={token} placeholder={t('common.token')} autocomplete="off" /></li>
  </ul>
</nav>

<main class="container">
  {#if route.view === 'bindings'}
    <Bindings {token} />
  {:else if route.view === 'create'}
    <CreateBinding {token} onCreated={() => (location.hash = '#/bindings')} />
  {:else if route.view === 'settings'}
    <Settings {token} />
  {:else if route.view === 'chat'}
    <ChatLayout name={route.arg || ''} {token} />
  {/if}
</main>

<style>
  .topnav { padding: 0 1rem; border-bottom: 1px solid var(--pico-muted-border-color); position: sticky; top: 0; background: var(--pico-background-color); z-index: 10; flex-wrap: wrap; gap: .25rem; }
  .topnav ul { margin: 0; flex-wrap: wrap; }
  .topnav a { padding: .4rem .6rem; border-radius: var(--pico-border-radius); text-decoration: none; }
  .topnav a.active { background: var(--pico-primary-background); color: var(--pico-primary-inverse); }
  .muted { color: var(--pico-muted-color); }
  .themebtn { width: auto; padding: .2rem .45rem; margin: 0; background: transparent; border: 1px solid var(--pico-muted-border-color); border-radius: var(--pico-border-radius); line-height: 1; cursor: pointer; }
  .lang { width: auto; font-size: .75rem; padding: .15rem 1.4rem .15rem .4rem; margin: 0; }
  .tok { width: 120px; font-size: .75rem; padding: .2rem .4rem; margin: 0; }
  main.container { max-width: 1280px; padding-top: 1.2rem; }

  /* Mobile: stack the brand above a wrapping link row; token input goes
     full-width on its own line so the nav never overflows. */
  @media (max-width: 640px) {
    .topnav { padding: .25rem .6rem; }
    .topnav .brand { width: 100%; }
    .topnav .links { width: 100%; justify-content: flex-start; gap: .15rem; }
    .topnav a { padding: .45rem .55rem; font-size: .9rem; }
    .tok-li { flex: 1 1 100%; }
    .tok { width: 100%; }
    main.container { padding-top: .6rem; }
  }
</style>
