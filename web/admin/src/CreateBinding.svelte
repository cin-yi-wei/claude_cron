<script>
  import { sendJSON } from './lib/api.js';
  let { token, onCreated } = $props();
  let kind = $state('worker'); // 'worker' | 'control'
  let f = $state({ name: '', project_dir: '', branch: '', platform: '', mode: '', chat_id: '' });
  let err = $state('');
  let msg = $state('');

  async function create() {
    err = ''; msg = '';
    const body = { name: f.name.trim(), control: kind === 'control' };
    if (kind === 'worker') {
      body.project_dir = f.project_dir.trim();
      body.branch = f.branch.trim();
      if (f.mode) body.mode = f.mode;
    }
    if (f.platform) body.platform = f.platform;
    if (f.chat_id) body.chat_id = f.chat_id.trim();
    try {
      const j = await sendJSON(token, 'POST', '/api/bindings', body);
      msg = j.result || 'created';
      f = { name: '', project_dir: '', branch: '', platform: '', mode: '', chat_id: '' };
      onCreated?.();
    } catch (e) { err = String(e); }
  }
</script>

<article>
  <header><strong>Create</strong></header>
  <div role="group" class="tabs">
    <button class={kind === 'worker' ? '' : 'outline'} onclick={() => (kind = 'worker')}>Worker</button>
    <button class={kind === 'control' ? '' : 'outline'} onclick={() => (kind = 'control')}>Control</button>
  </div>
  {#if err}<p class="bad">{err}</p>{/if}
  {#if msg}<p class="ok">{msg}</p>{/if}

  <div class="grid">
    <label>name <input bind:value={f.name} placeholder="a-z0-9-" /></label>
    <label>platform
      <select bind:value={f.platform}>
        {#if kind === 'control'}<option value="">web (預設)</option>{:else}<option value="">discord (預設)</option>{/if}
        {#if kind === 'control'}<option value="web">web</option>{/if}
        <option value="dc">discord</option>
        <option value="tg">telegram</option>
      </select>
    </label>
  </div>

  {#if kind === 'worker'}
    <div class="grid">
      <label>project dir <input bind:value={f.project_dir} placeholder="/path/to/repo" /></label>
      <label>branch <input bind:value={f.branch} placeholder="dev" /></label>
    </div>
    <div class="grid">
      <label>mode
        <select bind:value={f.mode}><option value="">poll (預設)</option><option value="push">push</option></select>
      </label>
      <label>chat-id (tg) <input bind:value={f.chat_id} /></label>
    </div>
  {:else}
    <div class="grid">
      <label>chat-id (tg control 用) <input bind:value={f.chat_id} /></label>
      <div></div>
    </div>
    <p class="muted"><small>Control 不用綁目錄/分支。第一個建立的 control 會成為受保護的預設🛡。</small></p>
  {/if}

  <button onclick={create}>Create {kind}</button>
</article>

<style>
  .muted { color: var(--pico-muted-color); }
  .bad { color: var(--pico-del-color); }
  .ok { color: var(--pico-ins-color); }
  label { font-size: .8rem; color: var(--pico-muted-color); }
  button { width: auto; }
  .tabs { max-width: 280px; margin-bottom: .6rem; }
</style>
