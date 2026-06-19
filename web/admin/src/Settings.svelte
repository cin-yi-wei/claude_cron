<script>
  import { getJSON, sendJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';
  let { token } = $props();
  let c = $state(null);
  let err = $state('');
  let msg = $state('');
  // editable fields
  let f = $state({ discord_transport: 'gateway', telegram_transport: 'webhook', push_listen: '', push_public_url: '', telegram_chat_id: '', push_secret: '', discord_token: '', telegram_token: '' });

  async function load() {
    err = '';
    try {
      c = await getJSON(token, '/api/config');
      f.discord_transport = c.discord_transport;
      f.telegram_transport = c.telegram_transport;
      f.push_listen = c.push_listen || '';
      f.push_public_url = c.push_public_url || '';
      f.telegram_chat_id = c.telegram_chat_id || '';
    } catch (e) { err = String(e); }
  }

  async function save() {
    err = ''; msg = '';
    const body = {
      discord_transport: f.discord_transport,
      telegram_transport: f.telegram_transport,
      push_listen: f.push_listen,
      push_public_url: f.push_public_url,
      telegram_chat_id: f.telegram_chat_id,
    };
    if (f.push_secret) body.push_secret = f.push_secret;
    if (f.discord_token) body.discord_token = f.discord_token;
    if (f.telegram_token) body.telegram_token = f.telegram_token;
    if (!confirm(t('settings.confirm'))) return;
    try {
      const j = await sendJSON(token, 'PUT', '/api/config', body);
      msg = j.restarting ? t('settings.saved.restarting') : t('settings.saved.manual');
    } catch (e) { err = String(e); }
  }

  $effect(() => { load(); });
</script>

<article>
  <header><strong>{t('settings.title')}</strong> <small class="muted">{t('settings.note')}</small></header>
  {#if err}<p class="bad">{err}</p>{/if}
  {#if msg}<p class="ok">{msg}</p>{/if}
  {#if c}
    <div class="grid">
      <label>discord transport
        <select bind:value={f.discord_transport}><option>gateway</option><option>poll</option></select>
      </label>
      <label>telegram transport
        <select bind:value={f.telegram_transport}><option>webhook</option><option>poll</option></select>
      </label>
    </div>
    <div class="grid">
      <label>push listen <input bind:value={f.push_listen} placeholder="127.0.0.1:8788" /></label>
      <label>push public_url <input bind:value={f.push_public_url} placeholder="https://…/tg" /></label>
    </div>
    <div class="grid">
      <label>push secret <input type="password" bind:value={f.push_secret} placeholder={c.push_secret_set ? '(set — 留空=不變)' : '(none)'} autocomplete="off" /></label>
      <label>tg control chat-id <input bind:value={f.telegram_chat_id} /></label>
    </div>
    <div class="grid">
      <label>discord bot token <input type="password" bind:value={f.discord_token} placeholder={c.discord_token_set ? '(set — 留空=不變)' : '(none)'} autocomplete="off" /></label>
      <label>telegram bot token <input type="password" bind:value={f.telegram_token} placeholder={c.telegram_token_set ? '(set — 留空=不變)' : '(none)'} autocomplete="off" /></label>
    </div>
    <button onclick={save}>{t('settings.save')}</button>
  {:else}
    <p class="muted"><em>{t('common.loading')}</em></p>
  {/if}
</article>

<style>
  .muted { color: var(--pico-muted-color); }
  .bad { color: var(--pico-del-color); }
  .ok { color: var(--pico-ins-color); }
  label { font-size: .8rem; color: var(--pico-muted-color); }
  button { width: auto; }
</style>
