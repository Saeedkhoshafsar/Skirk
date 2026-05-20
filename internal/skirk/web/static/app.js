/*
 * Skirk Web UI — Alpine.js application.
 *
 * This file holds all the dashboard logic for PR #2 step 2-a:
 *   - i18n loader with simple "{var}" interpolation
 *   - theme manager (auto / light / dark, persisted in localStorage)
 *   - session bootstrap (cookie + #token=… URL hash + /api/login)
 *   - fetch helper with CSRF, 401 / 429 / 5xx mapping → toasts
 *   - data loaders for /api/status, /api/client-profile, /api/mailboxes
 *   - modal flows: add (UI stub for now), rename, remove, promote
 *
 * No build step. Loaded as a plain <script src="/app.js"> alongside
 * Alpine.js v3 — see internal/skirk/web/static/index.html.
 */

// ---- Constants ----------------------------------------------------------

const MAX_EXTRAS = 15;                       // mirrors backend limit
const LABEL_RE = /^[a-zA-Z0-9._-]{1,64}$/;   // mirrors looksLikeSafeLabel()
const SUPPORTED_LANGS = ['en', 'fa'];
const DEFAULT_LANG = 'fa';                   // FA-first project
const STORAGE_LANG = 'skirk.lang';
const STORAGE_THEME = 'skirk.theme';

// ---- Helpers ------------------------------------------------------------

function pickInitialLang() {
  // 1) explicit user choice from previous visits
  const saved = localStorage.getItem(STORAGE_LANG);
  if (saved && SUPPORTED_LANGS.includes(saved)) return saved;
  // 2) browser preference (only the language tag, e.g. "fa-IR" → "fa")
  const navLang = (navigator.language || '').toLowerCase().split('-')[0];
  if (SUPPORTED_LANGS.includes(navLang)) return navLang;
  // 3) project default
  return DEFAULT_LANG;
}

function readTokenFromHash() {
  // Accept #token=XYZ to allow easy bootstrap (kept out of history).
  if (!location.hash || !location.hash.startsWith('#token=')) return '';
  const raw = location.hash.slice('#token='.length);
  // Wipe the hash so the token doesn't linger in the URL bar.
  history.replaceState(null, '', location.pathname + location.search);
  try { return decodeURIComponent(raw); } catch { return raw; }
}

function getCookie(name) {
  const prefix = name + '=';
  for (const part of document.cookie.split(';')) {
    const trimmed = part.trim();
    if (trimmed.startsWith(prefix)) return decodeURIComponent(trimmed.slice(prefix.length));
  }
  return '';
}

// Replace "{key}" placeholders in a translated string.
function interpolate(str, vars) {
  if (!vars) return str;
  return str.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
}

// Apply theme attribute + emit a CSS variable update.
function applyTheme(mode) {
  const html = document.documentElement;
  if (mode === 'auto') {
    html.removeAttribute('data-theme');
  } else {
    html.setAttribute('data-theme', mode);
  }
}

// ---- The Alpine component ----------------------------------------------

function app() {
  return {
    // ---- View state ---------------------------------------------------
    view: 'loading',          // 'loading' | 'login' | 'app'
    lang: DEFAULT_LANG,
    dict: {},                 // active translation bundle
    theme: localStorage.getItem(STORAGE_THEME) || 'auto',

    // ---- Login state --------------------------------------------------
    loginToken: '',
    loginBusy: false,
    loginError: '',
    loginPrefilled: false,

    // ---- Dashboard data ----------------------------------------------
    status: null,
    clientProfile: '',
    mailboxes: [],
    loading: false,
    busy: false,                       // mutating-call in flight

    // ---- Modal state --------------------------------------------------
    modal: null,                       // 'add' | 'edit' | 'confirm' | null
    addForm: { label: '', oauth_mode: 'easy', client_id: '', client_secret: '' },
    addLabelError: '',
    addShowCliHint: false,
    editForm: { target: '', newLabel: '' },
    editLabelError: '',
    confirmKind: '',                   // 'remove' | 'promote'
    confirmTarget: '',

    // ---- Toasts -------------------------------------------------------
    toasts: [],
    _toastId: 0,

    // ---- Constants exposed to templates ------------------------------
    MAX_EXTRAS,

    // ================================================================
    // Lifecycle
    // ================================================================

    async init() {
      this.theme = localStorage.getItem(STORAGE_THEME) || 'auto';
      applyTheme(this.theme);

      this.lang = pickInitialLang();
      await this.loadLang(this.lang);

      // If a token was passed via #token=, pre-fill the login form.
      const hashToken = readTokenFromHash();
      if (hashToken) {
        this.loginToken = hashToken;
        this.loginPrefilled = true;
      }

      // Decide login vs app based on whether we already have a session cookie.
      // The cleanest probe is calling /api/status; if it returns 200 we are
      // logged in (or the server is in --no-auth mode), 401 → show login.
      try {
        const r = await fetch('/api/status', { credentials: 'same-origin' });
        if (r.ok) {
          this.status = await r.json();
          this.view = 'app';
          await this.refreshAll({ skipStatus: true });
        } else if (r.status === 401) {
          this.view = 'login';
        } else {
          this.view = 'login';
          this.loginError = this.t('login.error.generic');
        }
      } catch (e) {
        this.view = 'login';
        this.loginError = this.t('toast.error.network');
      }
    },

    // ================================================================
    // i18n
    // ================================================================

    async loadLang(code) {
      if (!SUPPORTED_LANGS.includes(code)) code = DEFAULT_LANG;
      try {
        const r = await fetch(`/i18n/${code}.json`, { credentials: 'same-origin' });
        if (!r.ok) throw new Error('http ' + r.status);
        this.dict = await r.json();
      } catch (e) {
        // Fallback to a tiny inline dict so the UI is never empty.
        this.dict = { 'app.title': 'Skirk' };
      }
      this.lang = code;
      const meta = this.dict._meta || {};
      const html = document.documentElement;
      html.setAttribute('lang', meta.code || code);
      html.setAttribute('dir', meta.dir || (code === 'fa' ? 'rtl' : 'ltr'));
      document.title = this.t('app.title');
    },

    async setLang(code) {
      if (code === this.lang) return;
      localStorage.setItem(STORAGE_LANG, code);
      await this.loadLang(code);
    },

    t(key) { return (this.dict && this.dict[key]) || key; },
    tt(key, vars) { return interpolate(this.t(key), vars); },

    // ================================================================
    // Theme
    // ================================================================

    setTheme(mode) {
      this.theme = mode;
      if (mode === 'auto') localStorage.removeItem(STORAGE_THEME);
      else localStorage.setItem(STORAGE_THEME, mode);
      applyTheme(mode);
    },

    // ================================================================
    // Auth
    // ================================================================

    async doLogin() {
      this.loginError = '';
      const token = (this.loginToken || '').trim();
      if (!token) { this.loginError = this.t('login.error.empty'); return; }
      this.loginBusy = true;
      try {
        const r = await fetch('/api/login', {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ token }),
        });
        if (r.ok) {
          this.loginToken = '';
          this.view = 'app';
          await this.refreshAll();
          return;
        }
        if (r.status === 429) this.loginError = this.t('login.error.rate_limited');
        else if (r.status === 401) this.loginError = this.t('login.error.invalid');
        else this.loginError = this.t('login.error.generic');
      } catch (e) {
        this.loginError = this.t('toast.error.network');
      } finally {
        this.loginBusy = false;
      }
    },

    async doLogout() {
      try {
        await fetch('/api/logout', {
          method: 'POST',
          credentials: 'same-origin',
          headers: this._csrfHeader(),
        });
      } catch (_) { /* ignore */ }
      this.status = null;
      this.clientProfile = '';
      this.mailboxes = [];
      this.view = 'login';
    },

    _csrfHeader() {
      const token = getCookie('skirk_csrf');
      const headers = {};
      if (token) headers['X-CSRF-Token'] = token;
      return headers;
    },

    // ================================================================
    // Data loaders
    // ================================================================

    async refreshAll(opts = {}) {
      this.loading = true;
      try {
        const tasks = [];
        if (!opts.skipStatus) tasks.push(this.loadStatus());
        tasks.push(this.loadClientProfile());
        tasks.push(this.loadMailboxes());
        await Promise.all(tasks);
      } finally {
        this.loading = false;
      }
    },

    async loadStatus() {
      const data = await this._get('/api/status');
      if (data) this.status = data;
    },

    async loadClientProfile() {
      const data = await this._get('/api/client-profile');
      if (data && typeof data.client_profile === 'string') {
        this.clientProfile = data.client_profile;
      } else if (data && typeof data.profile === 'string') {
        // tolerate alt field name
        this.clientProfile = data.profile;
      } else {
        this.clientProfile = '';
      }
    },

    async loadMailboxes() {
      const data = await this._get('/api/mailboxes');
      if (data && Array.isArray(data.mailboxes)) {
        this.mailboxes = data.mailboxes;
      } else if (Array.isArray(data)) {
        this.mailboxes = data;
      } else {
        this.mailboxes = [];
      }
    },

    // ---- low-level fetch helpers --------------------------------------

    async _get(path) {
      try {
        const r = await fetch(path, { credentials: 'same-origin' });
        if (r.status === 401) { this._onUnauthorized(); return null; }
        if (!r.ok) {
          this._toastError(r.status);
          return null;
        }
        return await r.json();
      } catch (e) {
        this.pushToast('error', this.t('toast.error.network'));
        return null;
      }
    },

    async _mutate(method, path, body) {
      try {
        const r = await fetch(path, {
          method,
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json', ...this._csrfHeader() },
          body: body == null ? undefined : JSON.stringify(body),
        });
        if (r.status === 401) { this._onUnauthorized(); return { ok: false }; }
        if (!r.ok) {
          this._toastError(r.status);
          // Surface server-provided message if available.
          try {
            const data = await r.json();
            if (data && data.message) {
              this.pushToast('error', data.message);
            }
          } catch (_) { /* ignore */ }
          return { ok: false, status: r.status };
        }
        const ct = r.headers.get('Content-Type') || '';
        const data = ct.includes('json') ? await r.json().catch(() => null) : null;
        return { ok: true, status: r.status, data };
      } catch (e) {
        this.pushToast('error', this.t('toast.error.network'));
        return { ok: false };
      }
    },

    _toastError(status) {
      let key = 'toast.error.generic';
      if (status === 403) key = 'toast.error.forbidden';
      else if (status === 404) key = 'toast.error.not_found';
      else if (status === 409 || status === 422) key = 'toast.error.conflict';
      else if (status === 429) key = 'toast.error.rate_limited';
      else if (status >= 500) key = 'toast.error.server';
      this.pushToast('error', this.t(key));
    },

    _onUnauthorized() {
      this.pushToast('error', this.t('toast.error.unauthorized'));
      this.status = null;
      this.clientProfile = '';
      this.mailboxes = [];
      this.view = 'login';
    },

    // ================================================================
    // Computed-ish helpers used by templates
    // ================================================================

    get extrasCount() {
      return this.mailboxes.filter(m => !m.is_primary).length;
    },

    oauthLabel(mode) {
      if (mode === 'easy') return this.t('mailboxes.oauth.easy');
      if (mode === 'personal') return this.t('mailboxes.oauth.personal');
      return this.t('mailboxes.oauth.unknown');
    },

    truncMiddle(s, max) {
      if (!s) return '—';
      if (s.length <= max) return s;
      const keep = Math.max(4, Math.floor((max - 1) / 2));
      return s.slice(0, keep) + '…' + s.slice(s.length - keep);
    },

    formatUptime(seconds) {
      if (seconds == null || isNaN(seconds)) return this.t('common.unknown');
      seconds = Math.max(0, Math.floor(seconds));
      if (seconds < 60) return this.tt('uptime.seconds', { n: seconds });
      const m = Math.floor(seconds / 60);
      if (m < 60) return this.tt('uptime.minutes', { n: m });
      const h = Math.floor(m / 60);
      const remM = m - h * 60;
      if (h < 24) return this.tt('uptime.hours', { n: h, m: remM });
      const d = Math.floor(h / 24);
      const remH = h - d * 24;
      return this.tt('uptime.days', { n: d, h: remH });
    },

    // ================================================================
    // Clipboard
    // ================================================================

    async copy(text, okKey) {
      try {
        await navigator.clipboard.writeText(text);
        this.pushToast('success', this.t(okKey || 'toast.copied'));
      } catch (_) {
        // Fallback: temporary textarea + execCommand. Best-effort, no toast on failure.
        try {
          const ta = document.createElement('textarea');
          ta.value = text;
          ta.style.position = 'fixed';
          ta.style.opacity = '0';
          document.body.appendChild(ta);
          ta.focus(); ta.select();
          document.execCommand('copy');
          document.body.removeChild(ta);
          this.pushToast('success', this.t(okKey || 'toast.copied'));
        } catch (_e) { /* swallow */ }
      }
    },

    // ================================================================
    // Add Mailbox modal (UI-only in step 2-a)
    // ================================================================

    openAddModal() {
      this.addForm = { label: '', oauth_mode: 'easy', client_id: '', client_secret: '' };
      this.addLabelError = '';
      this.addShowCliHint = false;
      this.modal = 'add';
    },

    validateAddLabel() {
      const v = (this.addForm.label || '').trim();
      if (!v) { this.addLabelError = ''; return; }
      if (!LABEL_RE.test(v)) { this.addLabelError = this.t('add.step.label.invalid'); return; }
      const exists = this.mailboxes.some(m => (m.label || '').toLowerCase() === v.toLowerCase());
      if (exists) { this.addLabelError = this.t('add.step.label.duplicate'); return; }
      this.addLabelError = '';
    },

    submitAdd() {
      // Step 2-a behavior: validation only; the actual OAuth+POST flow
      // arrives in step 2-b. We just surface a friendly CLI hint.
      this.validateAddLabel();
      if (this.addLabelError || !this.addForm.label) return;
      this.addShowCliHint = true;
    },

    // ================================================================
    // Edit (rename) modal
    // ================================================================

    openEditModal(mb) {
      this.editForm = { target: mb.label || '', newLabel: mb.label || '' };
      this.editLabelError = '';
      this.modal = 'edit';
    },

    validateEditLabel() {
      const v = (this.editForm.newLabel || '').trim();
      if (!v) { this.editLabelError = ''; return; }
      if (!LABEL_RE.test(v)) { this.editLabelError = this.t('add.step.label.invalid'); return; }
      // Allow keeping the same label (will be blocked by submit button anyway).
      const dup = this.mailboxes.some(m =>
        (m.label || '').toLowerCase() === v.toLowerCase() &&
        (m.label || '') !== this.editForm.target);
      if (dup) { this.editLabelError = this.t('add.step.label.duplicate'); return; }
      this.editLabelError = '';
    },

    async submitEdit() {
      this.validateEditLabel();
      if (this.editLabelError || !this.editForm.newLabel) return;
      this.busy = true;
      const path = `/api/mailboxes/${encodeURIComponent(this.editForm.target)}`;
      const res = await this._mutate('PATCH', path, { label: this.editForm.newLabel });
      this.busy = false;
      if (res.ok) {
        this.pushToast('success', this.tt('toast.renamed', { label: this.editForm.newLabel }));
        this.closeModal();
        await this.loadMailboxes();
      }
    },

    // ================================================================
    // Confirm modal (remove / promote)
    // ================================================================

    openRemoveModal(mb) {
      this.confirmKind = 'remove';
      this.confirmTarget = mb.label || '';
      this.modal = 'confirm';
    },

    openPromoteModal(mb) {
      this.confirmKind = 'promote';
      this.confirmTarget = mb.label || '';
      this.modal = 'confirm';
    },

    async submitConfirm() {
      if (!this.confirmTarget) return;
      this.busy = true;
      const enc = encodeURIComponent(this.confirmTarget);
      let res;
      if (this.confirmKind === 'remove') {
        res = await this._mutate('DELETE', `/api/mailboxes/${enc}`);
      } else if (this.confirmKind === 'promote') {
        res = await this._mutate('POST', `/api/mailboxes/${enc}/promote`);
      }
      this.busy = false;
      if (res && res.ok) {
        const key = this.confirmKind === 'remove' ? 'toast.removed' : 'toast.promoted';
        this.pushToast('success', this.tt(key, { label: this.confirmTarget }));
        this.closeModal();
        await Promise.all([this.loadMailboxes(), this.loadStatus(), this.loadClientProfile()]);
      }
    },

    closeModal() {
      this.modal = null;
    },

    // ================================================================
    // Toasts
    // ================================================================

    pushToast(kind, msg) {
      const id = ++this._toastId;
      this.toasts.push({ id, kind, msg });
      // Auto-dismiss after 4.5s.
      setTimeout(() => {
        this.toasts = this.toasts.filter(t => t.id !== id);
      }, 4500);
    },
  };
}

// Expose globally so Alpine's x-data="app()" can find it (Alpine evaluates
// the expression in the global scope).
window.app = app;
