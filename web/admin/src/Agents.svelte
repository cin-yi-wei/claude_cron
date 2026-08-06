<script>
  import { getJSON, sendJSON } from './lib/api.js';
  import { t } from './lib/i18n.svelte.js';

  let { token } = $props();
  let tab = $state('agents');
  let agents = $state([]);
  let callers = $state([]);
  let tasks = $state([]);
  let err = $state('');
  let msg = $state('');
  let loading = $state(false);

  // /api/a2a/* 404 表示整個 kill switch 是關的（cfg.A2A.Enabled == false），
  // 不是「這條路由不存在」的一般錯誤 —— 換成好懂的提示而不是丟原始錯誤字串。
  async function load() {
    err = ''; loading = true;
    try {
      if (tab === 'agents') agents = await getJSON(token, '/api/a2a/agents');
      if (tab === 'callers') callers = await getJSON(token, '/api/a2a/callers');
      if (tab === 'tasks') tasks = await getJSON(token, '/api/a2a/tasks');
    } catch (e) {
      const s = String(e);
      // 404 也可能是伺服器讀設定檔失敗（a2a_admin.go 把「讀不到」跟「沒啟用」
      // 當成同一件事），所以訊息不能斷言原因。
      err = s.includes('404') ? t('agents.unavailable') : s;
    }
    loading = false;
  }
  $effect(() => { tab; token; load(); });

  // 切分頁：離開 callers 分頁就把已顯示的一次性憑證從畫面上清掉，
  // 不留在別的分頁還看得到的狀態裡。放在 selectTab 而不是 $effect，
  // 避免依賴 newCredential 在腳本裡宣告的先後順序。
  function selectTab(id) {
    tab = id;
    if (id !== 'callers') newCredential = null;
  }

  async function post(url) {
    err = ''; msg = '';
    try { const j = await sendJSON(token, 'POST', url, null); msg = j.status || 'ok'; await load(); }
    catch (e) { err = String(e); }
  }
  async function del(url) {
    err = ''; msg = '';
    try { const j = await sendJSON(token, 'DELETE', url, null); msg = j.status || 'ok'; await load(); }
    catch (e) { err = String(e); }
  }
  // 等級順序：只有「往上調」需要確認。full 等同把主機交出去（無限制 Bash
  // 加 MCP），而這是這個頁面上唯一一個會擴權的控制項——撤銷跟刪除都有確認，
  // 它不能沒有。往下調是收緊權限，不擋。
  const levelRank = { readonly: 1, develop: 2, full: 3 };
  async function setLevel(id, level, from) {
    if ((levelRank[level] || 0) > (levelRank[from] || 0)) {
      if (!confirm(t('agents.confirmRaiseLevel', { id, level }))) {
        await load(); // 使用者取消：把 select 拉回伺服器實際持有的值
        return;
      }
    }
    err = '';
    try { await sendJSON(token, 'POST', `/api/a2a/callers/${encodeURIComponent(id)}/level`, { level }); }
    catch (e) { err = String(e); }
    // 成功或失敗都重讀。失敗時不重讀的話，select 會一直顯示使用者選的那個值，
    // 而伺服器持有的還是舊的——頁面等於在謊報誰有什麼權限。
    await load();
  }

  // submitting 擋住連點。註冊尤其要擋：兩次都成功的話，第二次的回應會蓋掉
  // newCredential，操作者複製到的憑證就屬於另一個呼叫方了。
  let submitting = $state(false);

  function parseCaps(s) {
    return s.split(',').map((x) => x.trim()).filter(Boolean);
  }

  // --- agents: create + destructive remove (needs confirmation) ---
  let addingAgent = $state(false);
  let af = $state({ name: '', project_dir: '', description: '', capabilities: '', enabled: true });

  async function createAgent() {
    if (submitting) return;
    submitting = true;
    err = ''; msg = '';
    try {
      await sendJSON(token, 'POST', '/api/a2a/agents', {
        name: af.name.trim(),
        project_dir: af.project_dir.trim(),
        description: af.description.trim(),
        capabilities: parseCaps(af.capabilities),
        enabled: af.enabled,
      });
      msg = t('agents.agent.created', { name: af.name });
      af = { name: '', project_dir: '', description: '', capabilities: '', enabled: true };
      addingAgent = false;
      load();
    } catch (e) { err = String(e); }
    finally { submitting = false; }
  }

  function removeAgent(name) {
    if (!confirm(t('agents.confirm.remove', { name }))) return;
    del(`/api/a2a/agents/${encodeURIComponent(name)}`);
  }

  // --- callers: register (shows a one-time credential), approve, revoke ---
  let addingCaller = $state(false);
  let cf = $state({ caller_id: '', credential: '' });

  // newCredential 只活在這個元件的記憶體裡：不寫 localStorage、不 console.log、
  // 換頁（unmount）或離開 callers 分頁就消失，且從此再也拿不回來 —— 跟後端
  // 「只在建立當下的回應裡出現一次」的承諾一致。
  let newCredential = $state(null); // { caller_id, credential } | null
  let copied = $state(false);

  async function registerCaller() {
    if (submitting) return;
    submitting = true;
    err = ''; msg = '';
    try {
      const j = await sendJSON(token, 'POST', '/api/a2a/callers', {
        caller_id: cf.caller_id.trim(),
        credential: cf.credential,
      });
      newCredential = { caller_id: j.caller_id, credential: j.credential };
      copied = false;
      cf = { caller_id: '', credential: '' };
      addingCaller = false;
      load();
    } catch (e) { err = String(e); }
    finally { submitting = false; }
  }

  async function copyCredential() {
    if (!newCredential) return;
    // 純 HTTP 之下 navigator.clipboard 是 undefined，原本這裡會靜靜什麼都不做，
    // 按鈕看起來壞掉。給明確回饋，文字本身仍然可以手動選取。
    try {
      if (!navigator.clipboard) throw new Error('clipboard unavailable');
      await navigator.clipboard.writeText(newCredential.credential);
      copied = true;
    } catch { err = t('agents.copyUnavailable'); }
  }

  function dismissCredential() {
    newCredential = null;
    copied = false;
  }

  let approveTarget = $state(''); // caller_id being approved, '' = dialog closed
  let approveCaps = $state('');

  // openApprove 同時服務「核准」與「修改已核准呼叫方的能力」——後端
  // /approve 本身就是可重複呼叫的：再次呼叫只是覆寫 GrantedCapabilities，
  // 狀態已經是 approved 的話維持 approved（a2a_callers.go 的 Approve 本來
  // 就是這樣寫的，不需要新路由）。existingCaps 帶進來預先填好輸入框，修改
  // 時不必先看純文字才回想起原本授權了什麼、也不會不小心用空字串蓋掉。
  function openApprove(id, existingCaps) {
    approveTarget = id;
    approveCaps = (existingCaps || []).join(', ');
  }
  function cancelApprove() { approveTarget = ''; }
  async function confirmApprove() {
    const id = approveTarget;
    const caps = parseCaps(approveCaps);
    // 空清單不是「不限」，是「這個呼叫方之後每一次呼叫都被拒絕」——這正是
    // 這裡要修的那個 bug 的根源（操作者以為留空比較寬鬆）。跟拉高授權等級
    // 一樣，唯一會讓呼叫方失去存取能力/擴大風險的動作才需要二次確認；縮小
    // 授權（填有東西）不擋。
    if (caps.length === 0 && !confirm(t('agents.confirmNoCaps', { id }))) {
      return; // 對話框留著開，操作者可以直接改再送
    }
    approveTarget = '';
    err = ''; msg = '';
    try {
      await sendJSON(token, 'POST', `/api/a2a/callers/${encodeURIComponent(id)}/approve`, { capabilities: caps });
      msg = t('agents.approve.done', { id });
      await load();
    } catch (e) { err = String(e); }
  }

  function revokeCaller(id) {
    if (!confirm(t('agents.confirm.revoke', { id }))) return;
    post(`/api/a2a/callers/${encodeURIComponent(id)}/revoke`);
  }

  function statusLabel(s) { return t('agents.status.' + s); }

  // --- tasks: cancel affects running work too, so confirm here as well ---
  function cancelTask(id) {
    if (!confirm(t('agents.confirm.cancel', { id }))) return;
    post(`/api/a2a/tasks/${encodeURIComponent(id)}/cancel`);
  }
</script>

<nav>
  <ul>
    {#each ['agents', 'callers', 'tasks'] as id}
      <li><a href="#/agents" class={tab === id ? 'active' : ''} onclick={() => selectTab(id)}>{t('agents.tab.' + id)}</a></li>
    {/each}
  </ul>
</nav>

{#if err}<p class="bad">{err}</p>{/if}
{#if msg}<p class="ok">{msg}</p>{/if}

{#if tab === 'agents'}
  <article>
    <header class="head-row">
      <span><strong>{t('agents.tab.agents')}</strong> ({agents.length})</span>
      <span class="head-controls">
        <button class="mini" onclick={load} aria-busy={loading}>↻</button>
        <button class="mini secondary" onclick={() => (addingAgent = !addingAgent)}>
          {addingAgent ? t('common.cancel') : t('agents.agent.add')}
        </button>
      </span>
    </header>
    <p><small class="muted">{t('agents.note.caps')}</small></p>

    {#if addingAgent}
      <div class="addform">
        <div class="grid">
          <label>{t('agents.col.name')} <input bind:value={af.name} placeholder="pm" /></label>
          <label>{t('agents.agent.project')} <input bind:value={af.project_dir} placeholder="/path/to/repo" /></label>
        </div>
        <label>{t('agents.agent.desc')} <input bind:value={af.description} /></label>
        <label>{t('agents.col.caps')} <input bind:value={af.capabilities} placeholder={t('agents.agent.capsPlaceholder')} /></label>
        <label class="checkline">
          <input type="checkbox" bind:checked={af.enabled} />
          {t('agents.agent.enabledLabel')}
        </label>
        <button onclick={createAgent} disabled={submitting}>{t('agents.agent.submit')}</button>
      </div>
    {/if}

    <div style="overflow-x:auto">
      <table>
        <thead><tr>
          <th>{t('agents.col.name')}</th><th>{t('agents.col.project')}</th>
          <th>{t('agents.col.caps')}</th><th>{t('agents.col.enabled')}</th><th></th>
        </tr></thead>
        <tbody>
          {#each agents as a}
            <tr>
              <td><strong>{a.name}</strong></td>
              <td><code>{a.project_dir}</code></td>
              <td>{(a.capabilities || []).join(', ')}</td>
              <td>{a.enabled ? '✅' : '—'}</td>
              <td class="actions">
                {#if a.enabled}
                  <button class="mini secondary" onclick={() => post(`/api/a2a/agents/${encodeURIComponent(a.name)}/disable`)}>{t('agents.action.disable')}</button>
                {:else}
                  <button class="mini secondary" onclick={() => post(`/api/a2a/agents/${encodeURIComponent(a.name)}/enable`)}>{t('agents.action.enable')}</button>
                {/if}
                <button class="mini contrast outline" onclick={() => removeAgent(a.name)}>{t('agents.action.remove')}</button>
              </td>
            </tr>
          {/each}
          {#if agents.length === 0}
            <tr><td colspan="5"><em class="muted">{t('common.none')}</em></td></tr>
          {/if}
        </tbody>
      </table>
    </div>
  </article>
{:else if tab === 'callers'}
  <article>
    <header class="head-row">
      <span><strong>{t('agents.tab.callers')}</strong> ({callers.length})</span>
      <span class="head-controls">
        <button class="mini" onclick={load} aria-busy={loading}>↻</button>
        <button class="mini secondary" onclick={() => (addingCaller = !addingCaller)}>
          {addingCaller ? t('common.cancel') : t('agents.caller.add')}
        </button>
      </span>
    </header>

    {#if newCredential}
      <article class="credential-banner">
        <header><strong>⚠ {t('agents.credential.title')}</strong></header>
        <p>{t('agents.credential.warning')}</p>
        <div class="credline">
          <span class="muted">{newCredential.caller_id}</span>
          <code>{newCredential.credential}</code>
        </div>
        <footer>
          <button class="mini secondary" onclick={copyCredential}>{copied ? t('agents.credential.copied') : t('agents.credential.copy')}</button>
          <button class="mini" onclick={dismissCredential}>{t('agents.credential.dismiss')}</button>
        </footer>
      </article>
    {/if}

    {#if addingCaller}
      <div class="addform">
        <label>{t('agents.caller.id')} <input bind:value={cf.caller_id} placeholder="peer-a" /></label>
        <label>{t('agents.caller.credentialOptional')} <input bind:value={cf.credential} autocomplete="off" /></label>
        <button onclick={registerCaller} disabled={submitting}>{t('agents.caller.submit')}</button>
      </div>
    {/if}

    <div style="overflow-x:auto">
      <table>
        <thead><tr>
          <th>{t('agents.col.caller')}</th><th>{t('agents.col.status')}</th>
          <th>{t('agents.col.level')}</th><th>{t('agents.col.caps')}</th>
          <th>{t('agents.col.credential')}</th>
          <th>{t('agents.col.callback')}</th><th></th>
        </tr></thead>
        <tbody>
          {#each callers as c}
            <tr>
              <td>{c.caller_id}</td>
              <td>{statusLabel(c.status)}</td>
              <td>
                <select value={c.grant_level} disabled={c.status === 'revoked'} onchange={(e) => setLevel(c.caller_id, e.currentTarget.value, c.grant_level)}>
                  {#each ['readonly', 'develop', 'full'] as l}<option value={l}>{l}</option>{/each}
                </select>
              </td>
              <td>
                {#if c.status === 'approved' && (c.granted_capabilities || []).length === 0}
                  <span class="bad">{t('agents.caller.noCaps')}</span>
                {:else}
                  {(c.granted_capabilities || []).join(', ') || '—'}
                {/if}
              </td>
              <td>{c.has_credential ? '✅' : '—'}</td>
              <td>{c.has_callback ? '✅' : '—'}</td>
              <td class="actions">
                {#if c.status !== 'revoked'}
                  <button class="mini secondary" onclick={() => openApprove(c.caller_id, c.granted_capabilities)}>
                    {c.status === 'pending' ? t('agents.action.approve') : t('agents.action.editCaps')}
                  </button>
                {/if}
                {#if c.status !== 'revoked'}
                  <button class="mini contrast outline" onclick={() => revokeCaller(c.caller_id)}>{t('agents.action.revoke')}</button>
                {/if}
              </td>
            </tr>
          {/each}
          {#if callers.length === 0}
            <tr><td colspan="7"><em class="muted">{t('common.none')}</em></td></tr>
          {/if}
        </tbody>
      </table>
    </div>
  </article>
{:else}
  <article>
    <header class="head-row">
      <span><strong>{t('agents.tab.tasks')}</strong> ({tasks.length})</span>
      <span class="head-controls">
        <button class="mini" onclick={load} aria-busy={loading}>↻</button>
      </span>
    </header>
    <div style="overflow-x:auto">
      <table>
        <thead><tr>
          <th>{t('agents.col.context')}</th><th>{t('agents.col.state')}</th>
          <th>{t('agents.col.level')}</th><th>{t('agents.col.started')}</th>
          <th>{t('agents.col.branch')}</th><th></th>
        </tr></thead>
        <tbody>
          {#each tasks as k}
            <tr>
              <td><code>{k.context_id}</code></td>
              <td>{k.state}</td>
              <td>{k.level}</td>
              <td>{k.started_at}</td>
              <td><code>{k.branch}</code></td>
              <td class="actions">
                {#if k.state !== 'completed' && k.state !== 'failed' && k.state !== 'canceled'}
                  <button class="mini contrast outline" onclick={() => cancelTask(k.context_id)}>{t('agents.action.cancel')}</button>
                {/if}
              </td>
            </tr>
          {/each}
          {#if tasks.length === 0}
            <tr><td colspan="6"><em class="muted">{t('common.none')}</em></td></tr>
          {/if}
        </tbody>
      </table>
    </div>
  </article>
{/if}

{#if approveTarget}
  <dialog open>
    <article>
      <header><strong>{t('agents.approve.title', { id: approveTarget })}</strong></header>
      <label>{t('agents.approve.caps')} <input bind:value={approveCaps} placeholder="plan, read" /></label>
      <footer>
        <button class="secondary" onclick={cancelApprove}>{t('common.cancel')}</button>
        <button onclick={confirmApprove}>{t('agents.approve.submit')}</button>
      </footer>
    </article>
  </dialog>
{/if}

<style>
  .bad { color: var(--pico-del-color); }
  .ok { color: var(--pico-ins-color); }
  .muted { color: var(--pico-muted-color); }
  code { word-break: break-all; }
  table { font-size: .95rem; }
  table td, table th { padding: .6rem .7rem; vertical-align: middle; }
  .actions { white-space: nowrap; }
  .mini { width: auto; padding: .3rem .6rem; font-size: .9rem; margin: 0 .15rem; }
  .head-row { display: flex; align-items: center; justify-content: space-between; gap: .5rem; flex-wrap: wrap; }
  .head-controls { display: flex; align-items: center; gap: .4rem; flex: 0 0 auto; }
  .addform { border: 1px solid var(--pico-muted-border-color); border-radius: var(--pico-border-radius); padding: .8rem 1rem; margin-bottom: 1rem; }
  .addform label { font-size: .8rem; color: var(--pico-muted-color); }
  .checkline { display: flex; align-items: center; gap: .4rem; font-size: .85rem; }
  .addform button { width: auto; }
  .credential-banner { border: 2px solid var(--pico-del-color); margin-bottom: 1rem; }
  .credline { display: flex; align-items: center; gap: .6rem; flex-wrap: wrap; margin: .5rem 0; }
  .credline code { background: var(--pico-code-background-color); padding: .3rem .5rem; border-radius: var(--pico-border-radius); user-select: all; }
</style>
