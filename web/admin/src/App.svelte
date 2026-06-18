<script>
  import Bindings from './Bindings.svelte';
  import Settings from './Settings.svelte';
  import CreateBinding from './CreateBinding.svelte';
  import Chat from './Chat.svelte';

  let token = $state(localStorage.getItem('cc_admin_token') || '');
  $effect(() => { localStorage.setItem('cc_admin_token', token); });

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
    { id: 'bindings', label: 'Bindings', href: '#/bindings' },
    { id: 'chat', label: 'Chat', href: '#/chat' },
    { id: 'create', label: 'Create', href: '#/create' },
    { id: 'settings', label: 'Settings', href: '#/settings' },
  ];

  let bindingsRef = $state(null);
</script>

<nav class="container-fluid topnav">
  <ul>
    <li><strong>claude_cron</strong> <small class="muted">admin</small></li>
  </ul>
  <ul>
    {#each nav as n}
      <li><a href={n.href} class={route.view === n.id ? 'active' : ''}>{n.label}</a></li>
    {/each}
    <li><input class="tok" type="password" bind:value={token} placeholder="token" autocomplete="off" /></li>
  </ul>
</nav>

<main class="container">
  {#if route.view === 'bindings'}
    <Bindings {token} bind:this={bindingsRef} />
  {:else if route.view === 'create'}
    <CreateBinding {token} onCreated={() => (location.hash = '#/bindings')} />
  {:else if route.view === 'settings'}
    <Settings {token} />
  {:else if route.view === 'chat'}
    {#if route.arg}
      <Chat name={route.arg} {token} />
      <p><a href="#/bindings">← 回 bindings</a></p>
    {:else}
      <article>
        <header><strong>Chat</strong></header>
        <p class="muted">從 <a href="#/bindings">Bindings</a> 清單按 💬 開一個 active session 的聊天。</p>
      </article>
    {/if}
  {/if}
</main>

<style>
  .topnav { padding: 0 1rem; border-bottom: 1px solid var(--pico-muted-border-color); position: sticky; top: 0; background: var(--pico-background-color); z-index: 10; }
  .topnav ul { margin: 0; }
  .topnav a { padding: .4rem .6rem; border-radius: var(--pico-border-radius); text-decoration: none; }
  .topnav a.active { background: var(--pico-primary-background); color: var(--pico-primary-inverse); }
  .muted { color: var(--pico-muted-color); }
  .tok { width: 130px; font-size: .75rem; padding: .2rem .4rem; margin: 0; }
  main.container { max-width: 1000px; padding-top: 1rem; }
</style>
