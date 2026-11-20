// Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Private local Chrome helper. Input may contain secrets; stdout is restricted
// to domain/status metadata. Never forward CDP errors or extension console logs.
import { setTimeout as delay } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';

class SyncError extends Error {}
const fail = () => { throw new SyncError('COOKIECLOUD_CHROME_UNAVAILABLE'); };
const list = value => [...new Set(String(value ?? '').split(/\r?\n/).map(s => s.trim().toLowerCase()).filter(Boolean))].sort();

export function mergeSettings(raw, settings, confirmed) {
  const previous = raw === undefined ? {} : JSON.parse(raw);
  if ((raw !== undefined && typeof raw !== 'string') || !previous || Array.isArray(previous) || typeof previous !== 'object') fail();
  const local = list(previous.domains), requested = list(settings.domains);
  if (!requested.length) fail();
  if (raw !== undefined && JSON.stringify(local) !== JSON.stringify(requested) && !confirmed) {
    const recognized = local.length <= 32 && local.every(s => s.length <= 253 && /^(?:[a-z0-9-]+\.)+[a-z0-9-]+$/.test(s));
    return { confirmation_required: true, local_domains: recognized ? local : [], local_scope_unrecognized: !recognized, local_scope_all_websites: !local.length };
  }
  return { raw: JSON.stringify({ ...previous, ...settings }) };
}

function localURL(value, schemes) {
  const url = new URL(value);
  if (!schemes.includes(url.protocol) || !['localhost', '127.0.0.1', '[::1]'].includes(url.hostname) || !url.port || url.username || url.password || url.search || url.hash) fail();
  return url;
}

export class CDP {
  constructor(socket) {
    this.socket = socket; this.sequence = 0; this.pending = new Map(); this.events = new Set();
    socket.addEventListener('message', event => {
      let data;
      try { data = JSON.parse(String(event.data)); } catch { return; }
      if (data.id) {
        const callback = this.pending.get(data.id);
        if (callback) { this.pending.delete(data.id); callback(data); }
      } else for (const listener of [...this.events]) listener(data);
    });
    socket.addEventListener('close', () => this.cancel());
  }
  static async connect(endpoint) {
    let url = localURL(endpoint, ['http:', 'ws:']);
    if (url.protocol === 'http:') {
      const result = await fetch(new URL('/json/version', url), { redirect: 'error', signal: AbortSignal.timeout(30000) });
      if (!result.ok) fail();
      url = localURL((await result.json()).webSocketDebuggerUrl, ['ws:']);
    }
    const socket = new WebSocket(url);
    try {
      await new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new SyncError()), 30000);
        socket.addEventListener('open', () => { clearTimeout(timer); resolve(); }, { once: true });
        socket.addEventListener('error', () => { clearTimeout(timer); reject(new SyncError()); }, { once: true });
      });
    } catch { socket.close(); throw new SyncError(); }
    return new CDP(socket);
  }
  send(method, params = {}, sessionId) {
    const id = ++this.sequence;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new SyncError()); }, 30000);
      this.pending.set(id, data => { clearTimeout(timer); data.error ? reject(new SyncError()) : resolve(data.result); });
      this.socket.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
  }
  event(method, sessionId) {
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.events.delete(listener); reject(new SyncError()); }, 30000);
      const listener = data => {
        if (data.method === method && data.sessionId === sessionId) {
          clearTimeout(timer); this.events.delete(listener); resolve();
        }
      };
      listener.cancel = () => { clearTimeout(timer); this.events.delete(listener); reject(new SyncError()); };
      this.events.add(listener);
    });
  }
  cancel() {
    for (const listener of [...this.events]) listener.cancel();
    for (const callback of this.pending.values()) callback({ error: true });
    this.pending.clear();
  }
  close() {
    this.cancel(); this.socket.close();
  }
  async evaluate(fn, argument, sessionId) {
    const response = await this.send('Runtime.evaluate', {
      expression: `(${fn.toString()})(${JSON.stringify(argument)})`,
      awaitPromise: true, returnByValue: true, userGesture: true,
    }, sessionId);
    if (response.exceptionDetails) fail();
    return response.result.value;
  }
  async until(fn, argument, sessionId) {
    const deadline = Date.now() + 30000;
    while (!await this.evaluate(fn, argument, sessionId)) {
      if (Date.now() >= deadline) fail();
      await delay(100);
    }
  }
  async navigate(sessionId, url) {
    const response = await this.send('Page.navigate', { url }, sessionId);
    if (response.errorText) fail();
    await this.until(expected => location.href === expected && document.readyState === 'complete', url, sessionId);
  }
}

export async function configure(input) {
  if (input.extension_id !== 'ffjiejobkoibkjlhjnlgmcnnigeelbdl' || input.storage_key !== 'COOKIE_SYNC_SETTING') fail();
  let cdp, targetId, stage = 'connect';
  try {
    cdp = await CDP.connect(input.cdp_url);
    stage = 'extension';
    const origin = `chrome-extension://${input.extension_id}/`;
    const prior = (await cdp.send('Target.getTargets')).targetInfos.filter(t => t.type === 'page' && t.url === origin + 'popup.html');
    ({ targetId } = await cdp.send('Target.createTarget', { url: 'about:blank', background: true }));
    const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
    await cdp.send('Page.enable', {}, sessionId);
    await cdp.navigate(sessionId, origin + 'popup.html');
    stage = 'read';
    const old = await cdp.evaluate(async key => {
      if (chrome.runtime.id !== 'ffjiejobkoibkjlhjnlgmcnnigeelbdl') throw new Error();
      const value = await chrome.storage.local.get(key);
      return { present: Object.hasOwn(value, key), raw: value[key] };
    }, input.storage_key, sessionId);
    const plan = mergeSettings(old.present ? old.raw : undefined, input.settings, input.confirm_domains);
    if (plan.confirmation_required) return { ok: true, ...plan };
    stage = 'write';
    const saved = await cdp.evaluate(async ({ key, previous, present, next }) => {
      const current = await chrome.storage.local.get(key);
      if (Object.hasOwn(current, key) !== present || current[key] !== previous) return false;
      await chrome.storage.local.set({ [key]: next });
      return (await chrome.storage.local.get(key))[key] === next;
    }, { key: input.storage_key, previous: old.raw, present: old.present, next: plan.raw }, sessionId);
    if (!saved) fail();
    stage = 'reload';
    // Refresh only extension configuration pages so an old form cannot save
    // its stale values later. Website tabs and all other storage keys stay put.
    for (const tab of prior) {
      const attached = await cdp.send('Target.attachToTarget', { targetId: tab.targetId, flatten: true });
      try { await cdp.send('Page.reload', {}, attached.sessionId); }
      finally { await cdp.send('Target.detachFromTarget', { sessionId: attached.sessionId }); }
    }
    await cdp.navigate(sessionId, origin + 'popup.html');
    await cdp.until(settings => {
      const fields = [...document.querySelectorAll('input, textarea')].map(n => n.value);
      return [settings.endpoint, settings.uuid, settings.domains].every(v => fields.includes(v));
    }, input.settings, sessionId);
    // The official extension performs collection/encryption/upload. No Cookie
    // values or network bodies are extracted by this helper.
    stage = 'upload';
    const dialog = cdp.event('Page.javascriptDialogOpening', sessionId); dialog.catch(() => {});
    const clicked = await cdp.evaluate(() => {
      const label = chrome.i18n.getMessage('test') || '测试';
      const button = [...document.querySelectorAll('button')].find(n => n.textContent.trim() === label);
      if (!button) return false;
      button.click(); return true;
    }, null, sessionId);
    if (clicked) { await dialog; await cdp.send('Page.handleJavaScriptDialog', { accept: true }, sessionId); }
    return { ok: true, configured: true, readback_verified: true, upload_attempted: clicked };
  } catch (error) {
    error.stage = stage; throw error;
  } finally {
    if (cdp) {
      if (targetId) try { await cdp.send('Target.closeTarget', { targetId }); } catch {}
      cdp.close();
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    let data = '';
    for await (const chunk of process.stdin) { data += chunk; if (data.length > 1048576) fail(); }
    process.stdout.write(JSON.stringify(await configure(JSON.parse(data))) + '\n');
  } catch (error) {
    process.stdout.write(JSON.stringify({ ok: false, error: { code: 'COOKIECLOUD_CHROME_UNAVAILABLE' }, stage: error.stage }) + '\n');
    process.exitCode = 1;
  }
}
