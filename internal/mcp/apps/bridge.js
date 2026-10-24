/* Standard MCP Apps transport. No credentials or direct API requests. */
(function () {
  'use strict';
  let nextID = 0, context = {}, initialized = false, parentOrigin = '*', pinnedOrigin = null;
  const pending = new Map(), handlers = new Map();
  const send = message => window.parent.postMessage({ jsonrpc: '2.0', ...message }, parentOrigin);
  const notify = (method, params) => send({ method, params });
  function request(method, params) {
    const id = ++nextID;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { pending.delete(id); reject(new Error('The chat app did not respond. Try again.')); }, 60000);
      pending.set(id, { resolve, reject, timer, method });
      send({ id, method, params });
    });
  }
  function hostContext(update) {
    context = { ...context, ...update };
    const theme = context.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    document.documentElement.dataset.theme = theme === 'dark' ? 'dark' : 'light';
    for (const [key, value] of Object.entries(context.styles?.variables || {})) {
      if (key.startsWith('--') && typeof value === 'string') document.documentElement.style.setProperty(key, value);
    }
  }
  window.addEventListener('message', event => {
    if (event.source !== window.parent || !event.data || event.data.jsonrpc !== '2.0') return;
    if (pinnedOrigin !== null && event.origin !== pinnedOrigin) return;
    const message = event.data;
    if (!message.method && pending.has(message.id)) {
      const wait = pending.get(message.id); pending.delete(message.id); clearTimeout(wait.timer);
      if (message.error) wait.reject(new Error(message.error.message || 'The chat app could not finish this action.'));
      else {
        // Opaque sandbox proxy origins cannot be used as targetOrigin.
        if (wait.method === 'ui/initialize') { pinnedOrigin = event.origin; parentOrigin = event.origin && event.origin !== 'null' ? event.origin : '*'; }
        wait.resolve(message.result);
      }
      return;
    }
    if (message.method === 'ui/notifications/host-context-changed') hostContext(message.params || {});
    if (message.method === 'ui/resource-teardown') {
      for (const wait of pending.values()) { clearTimeout(wait.timer); wait.reject(new Error('Closed')); }
      pending.clear(); send({ id: message.id, result: {} }); return;
    }
    const handler = handlers.get(message.method);
    if (handler) handler(message.params || {});
    else if (message.id !== undefined) send({ id: message.id, error: { code: -32601, message: 'Method not supported' } });
  });
  window.SHApp = {
    // Raw results retain host-only metadata (for example chart series).
    callToolResult: async (name, args = {}) => {
      const result = await request('tools/call', { name, arguments: args });
      if (result.isError) throw new Error('Could not load this data. Try again.');
      return result;
    },

    get hostContext() { return context; },
    on: (method, handler) => handlers.set('ui/notifications/' + method, handler),
    callTool: async (name, args = {}) => {
      const result = await request('tools/call', { name, arguments: args });
      if (result.isError) throw new Error(result.content?.find(item => item.type === 'text')?.text || 'Could not save. Try again.');
      if (result.structuredContent) return result.structuredContent;
      const text = result.content?.find(item => item.type === 'text')?.text;
      return text ? JSON.parse(text) : {};
    },
    // Callers must use a URL returned by a server tool, never visitor input.
    openLink: url => {
      if (new URL(url).protocol !== 'https:') return Promise.reject(new Error('Only HTTPS links can be opened.'));
      return request('ui/open-link', { url });
    },
    requestDisplayMode: mode => request('ui/request-display-mode', { mode }),
    start: async () => {
      hostContext({});
      const result = await request('ui/initialize', {
        protocolVersion: '2026-01-26', appInfo: { name: 'Simple Host', version: '1.0.0' },
        appCapabilities: { availableDisplayModes: ['inline', 'fullscreen'] }
      });
      window.SHApp.hostCapabilities = result.hostCapabilities || {};
      hostContext({ ...result.hostContext, hostInfo: result.hostInfo });
      initialized = true;
      notify('ui/notifications/initialized', {});
      const resize = () => {
        if (initialized) notify('ui/notifications/size-changed', { height: Math.ceil(document.body.getBoundingClientRect().height), width: document.documentElement.clientWidth });
      };
      new ResizeObserver(resize).observe(document.body);
      resize();
    }
  };
})();
