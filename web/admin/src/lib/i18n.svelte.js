// Tiny dependency-free i18n. A reactive locale ($state) + flat dictionaries.
// Components call t('key'); reading `locale` inside t() during render makes them
// re-render when the locale changes. Persisted to localStorage.

const dict = {
  'zh-TW': {
    'app.subtitle': 'bindings + 瀏覽器聊天窗',
    'nav.bindings': 'Bindings',
    'nav.chat': '聊天',
    'nav.create': '新增',
    'nav.settings': '設定',
    'common.token': 'token',
    'common.refresh': '重整',
    'common.loading': '載入中…（檢查 token）',
    'common.none': '無（檢查 token）',
    'bindings.title': 'Bindings',
    'bindings.col.name': '名稱',
    'bindings.col.kind': '類型',
    'bindings.col.session': 'session',
    'bindings.col.queue': '佇列',
    'bindings.col.actions': '操作',
    'bindings.paused': '⏸ 已暫停',
    'bindings.control': 'control',
    'chat.back': '← 回 bindings',
    'chat.pick': '從 Bindings 清單按 💬 開一個 active session 的聊天。',
    'chat.empty': '還沒有訊息。輸入後送出，會注入 cc-{name} session。',
    'chat.send': '送出',
    'chat.placeholder': '輸入訊息給 {name}…',
    'chat.status.connecting': '連線中…',
    'chat.status.live': '連線中',
    'chat.status.reconnecting': '重連中…',
    'settings.title': '設定',
    'settings.note': '（儲存會重啟 serve 套用）',
    'settings.save': '儲存並重啟 serve',
    'settings.confirm': '儲存設定並重啟 serve？',
    'settings.saved.restarting': '已儲存 — 正在重啟 serve…',
    'settings.saved.manual': '已儲存（需手動重啟）',
    'create.title': '新增',
    'create.worker': 'Worker',
    'create.control': 'Control',
    'create.name': '名稱',
    'create.platform': '平台',
    'create.dir': '專案目錄',
    'create.branch': '分支',
    'create.mode': 'mode',
    'create.chatid': 'chat-id（tg）',
    'create.controlHint': 'Control 不用綁目錄/分支。第一個建立的 control 會成為受保護的預設🛡。',
    'create.submit': '建立',
    'unbind.confirm': '確定解綁 {name}？',
  },
  en: {
    'app.subtitle': 'bindings + in-browser chat',
    'nav.bindings': 'Bindings',
    'nav.chat': 'Chat',
    'nav.create': 'Create',
    'nav.settings': 'Settings',
    'common.token': 'token',
    'common.refresh': 'Refresh',
    'common.loading': 'loading… (check token)',
    'common.none': 'none (check token)',
    'bindings.title': 'Bindings',
    'bindings.col.name': 'name',
    'bindings.col.kind': 'kind',
    'bindings.col.session': 'session',
    'bindings.col.queue': 'queue',
    'bindings.col.actions': 'actions',
    'bindings.paused': '⏸ paused',
    'bindings.control': 'control',
    'chat.back': '← back to bindings',
    'chat.pick': 'Open a chat from the Bindings list with the 💬 button on an active session.',
    'chat.empty': 'No messages yet. Send one to inject into the cc-{name} session.',
    'chat.send': 'Send',
    'chat.placeholder': 'message {name}…',
    'chat.status.connecting': 'connecting…',
    'chat.status.live': 'live',
    'chat.status.reconnecting': 'reconnecting…',
    'settings.title': 'Settings',
    'settings.note': '(saving restarts serve to apply)',
    'settings.save': 'Save & Restart serve',
    'settings.confirm': 'Save settings and restart serve?',
    'settings.saved.restarting': 'saved — restarting serve…',
    'settings.saved.manual': 'saved (restart manually)',
    'create.title': 'Create',
    'create.worker': 'Worker',
    'create.control': 'Control',
    'create.name': 'name',
    'create.platform': 'platform',
    'create.dir': 'project dir',
    'create.branch': 'branch',
    'create.mode': 'mode',
    'create.chatid': 'chat-id (tg)',
    'create.controlHint': 'Control needs no dir/branch. The first control created becomes the protected default 🛡.',
    'create.submit': 'Create',
    'unbind.confirm': 'unbind {name}?',
  },
};

export const LOCALES = [
  { id: 'zh-TW', label: '中文' },
  { id: 'en', label: 'EN' },
];

let locale = $state(localStorage.getItem('cc_lang') || 'zh-TW');

export function getLocale() {
  return locale;
}

export function setLocale(l) {
  if (dict[l]) {
    locale = l;
    localStorage.setItem('cc_lang', l);
  }
}

// t(key, vars?) — looks up the current locale (falls back to zh-TW then the key)
// and interpolates {name}-style placeholders.
export function t(key, vars) {
  const table = dict[locale] || dict['zh-TW'];
  let s = table[key] ?? dict['zh-TW'][key] ?? key;
  if (vars) {
    for (const k in vars) s = s.replaceAll('{' + k + '}', vars[k]);
  }
  return s;
}
