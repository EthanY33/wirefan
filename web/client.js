// wirefan demo client. vanilla JS, no build step.
// Three panels: connection / messages / stats. Plus a stress button.

(() => {
  'use strict';

  const STATS_CHANNEL = '_wirefan-stats';
  const STRESS_CONNS = 50;
  const STRESS_HOLD_MS = 10_000;

  // Peer identity is DECORATIVE ONLY. It derives a short label and a color
  // from the real server-issued socket_id so that two browser windows in the
  // same screenshot are visually distinguishable. It adds no server semantics
  // and is not a presence or membership feature (see CLAUDE.md, "Deferred").
  // The label is the last 4 chars of the socket_id the server already sent;
  // the color is a local hash of it. Nothing here is invented data.
  const PEER_COLORS = [
    '#9e470f', '#12615f', '#33357f', '#6e2050',
    '#27526e', '#6f520e', '#276030', '#9b2033',
  ];
  function deriveIdentity(sid) {
    if (!sid) return { short: '', color: 'var(--accent)' };
    let h = 2166136261;
    for (let i = 0; i < sid.length; i++) {
      h ^= sid.charCodeAt(i);
      h = Math.imul(h, 16777619);
    }
    return { short: sid.slice(-4), color: PEER_COLORS[(h >>> 0) % PEER_COLORS.length] };
  }

  // ---------------- DOM ----------------
  const $ = (id) => document.getElementById(id);
  const els = {
    apiKey: $('apiKey'),
    btnConnect: $('btnConnect'),
    btnDisconnect: $('btnDisconnect'),
    btnSubscribe: $('btnSubscribe'),
    btnPublish: $('btnPublish'),
    btnStress: $('btnStress'),
    statusPill: $('status-pill'),
    socketId: $('socketId'),
    wsEndpoint: $('wsEndpoint'),
    subList: $('subList'),
    channelName: $('channelName'),
    msgBody: $('msgBody'),
    log: $('log'),
    msgCount: $('msgCount'),
    stressStatus: $('stressStatus'),
    statsGrid: $('statsGrid'),
    statsCaption: $('statsCaption'),
    statsRaw: $('statsRaw'),
    statsAge: $('statsAge'),
    peerBadge: $('peerBadge'),
    peerBadgeId: $('peerBadgeId'),
  };

  // ---------------- state ----------------
  let ws = null;
  let socketId = null;
  const subscribed = new Set();
  let messageCount = 0;
  let stressActive = false;
  let lastStatsAt = 0;
  let logCleared = false;
  let statsSubPending = false;
  const BASE_TITLE = document.title;

  // ---------------- helpers ----------------
  const wsScheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const wsBase = `${wsScheme}://${location.host}/v1/connect`;

  function setStatus(state, label) {
    els.statusPill.dataset.state = state;
    els.statusPill.textContent = label;
  }

  function refreshSubList() {
    if (subscribed.size === 0) {
      els.subList.textContent = '—';
    } else {
      els.subList.textContent = [...subscribed].join(', ');
    }
  }

  function setUiConnected(connected) {
    els.btnConnect.disabled = connected;
    els.btnDisconnect.disabled = !connected;
    els.btnSubscribe.disabled = !connected;
    els.btnPublish.disabled = !connected;
    els.btnStress.disabled = !connected || stressActive;
    if (!connected) {
      els.socketId.textContent = '—';
      els.wsEndpoint.textContent = '—';
      subscribed.clear();
      refreshSubList();
      document.body.classList.remove('is-connected');
      document.documentElement.style.removeProperty('--peer-color');
      els.peerBadgeId.textContent = '';
      document.title = BASE_TITLE;
    }
  }

  // Build a log row using DOM APIs (no innerHTML &mdash; avoids XSS via frame data).
  function logFrame(kind, channel, body, fromSid) {
    if (!logCleared) {
      els.log.replaceChildren();
      logCleared = true;
    }
    const row = document.createElement('div');
    row.className = 'log-row';

    const tsEl = document.createElement('span');
    tsEl.className = 'ts';
    tsEl.textContent = new Date().toISOString().slice(11, 19);

    const bodyEl = document.createElement('span');
    bodyEl.className = 'body';

    const tagEl = document.createElement('span');
    let tagClass = 'evt';
    if (kind === 'system') tagClass = 'sys';
    else if (kind === 'error') tagClass = 'err';
    tagEl.className = `tag ${tagClass}`;
    tagEl.textContent = kind.toUpperCase();
    bodyEl.appendChild(tagEl);

    if (channel) {
      const chEl = document.createElement('span');
      chEl.className = 'ch';
      chEl.textContent = channel;
      bodyEl.appendChild(chEl);
    }

    if (fromSid) {
      const from = deriveIdentity(fromSid);
      const chip = document.createElement('span');
      chip.className = 'peer-chip';
      chip.style.background = from.color;
      chip.textContent = `from ·${from.short}`;
      chip.title = `socket_id ${fromSid}`;
      bodyEl.appendChild(chip);
    }

    bodyEl.appendChild(document.createTextNode(body));

    row.appendChild(tsEl);
    row.appendChild(bodyEl);

    // Newest at top.
    els.log.prepend(row);
    if (kind === 'event') {
      row.classList.add('log-row--new');
      row.addEventListener('animationend', () => row.classList.remove('log-row--new'), { once: true });
    }
    while (els.log.children.length > 200) {
      els.log.removeChild(els.log.lastChild);
    }
    if (kind === 'event') {
      messageCount++;
      els.msgCount.textContent = `${messageCount} received`;
    }
  }

  function send(obj) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return false;
    ws.send(JSON.stringify(obj));
    return true;
  }

  // ---------------- connect ----------------
  function readKey() {
    const fromInput = els.apiKey.value.trim();
    if (fromInput) return fromInput;
    const params = new URLSearchParams(location.search);
    const fromUrl = params.get('key');
    if (fromUrl) {
      els.apiKey.value = fromUrl;
      return fromUrl;
    }
    return '';
  }

  function connect() {
    const key = readKey();
    if (!key) {
      logFrame('error', '', 'no api key. paste one or use ?key=...');
      return;
    }
    const url = `${wsBase}?key=${encodeURIComponent(key)}`;
    els.wsEndpoint.textContent = url;
    setStatus('connecting', 'connecting');
    try {
      ws = new WebSocket(url);
    } catch (e) {
      setStatus('error', 'error');
      logFrame('error', '', String(e));
      return;
    }
    ws.onopen = () => {
      setStatus('connected', 'connected');
      setUiConnected(true);
      // Auto-subscribe to the stats system channel.
      statsSubPending = true;
      send({ type: 'subscribe', channel: STATS_CHANNEL });
    };
    ws.onmessage = (e) => {
      let frame;
      try { frame = JSON.parse(e.data); }
      catch (_) { logFrame('error', '', `non-json: ${e.data}`); return; }
      handleFrame(frame);
    };
    ws.onerror = () => {
      setStatus('error', 'error');
    };
    ws.onclose = (e) => {
      setStatus('idle', 'idle');
      setUiConnected(false);
      if (e.reason) {
        logFrame('system', '', `closed: ${e.reason || e.code}`);
      }
      ws = null;
      socketId = null;
      statsSubPending = false;
    };
  }

  function disconnect() {
    if (ws) ws.close(1000, 'user-disconnect');
  }

  // Frames use "type" as the discriminator, matching the server
  // (internal/conn/handler.go) and docs/PROTOCOL.md.
  function handleFrame(frame) {
    const t = frame.type;
    if (t === 'connected') {
      socketId = frame.socket_id || null;
      els.socketId.textContent = socketId || '—';
      const me = deriveIdentity(socketId);
      document.documentElement.style.setProperty('--peer-color', me.color);
      els.peerBadgeId.textContent = `·${me.short}`;
      document.body.classList.add('is-connected');
      document.title = `wirefan · ${me.short}`;
      logFrame('system', '', `connected sid=${socketId || '?'}`);
      return;
    }
    if (t === 'subscribed') {
      if (frame.channel === STATS_CHANNEL) statsSubPending = false;
      subscribed.add(frame.channel);
      refreshSubList();
      logFrame('system', frame.channel, 'subscribed');
      return;
    }
    if (t === 'unsubscribed') {
      subscribed.delete(frame.channel);
      refreshSubList();
      logFrame('system', frame.channel, 'unsubscribed');
      return;
    }
    if (t === 'error') {
      // Error frames carry code+message but no channel. If the only
      // subscribe in flight is the automatic stats-channel one and the
      // server rejects it as reserved, degrade the stats panel honestly
      // and keep the demo log free of an expected, self-inflicted error.
      if (frame.code === 'RESERVED_CHANNEL' && statsSubPending) {
        statsSubPending = false;
        els.statsCaption.textContent = 'client subscription to the _wirefan-stats reserved channel is not enabled on this server build.';
        els.statsGrid.style.display = 'none';
        els.statsRaw.textContent = 'stats channel not open to clients on this server build';
        return;
      }
      const msg = frame.code ? `${frame.code}: ${frame.message || ''}` : (frame.message || JSON.stringify(frame));
      logFrame('error', '', msg);
      return;
    }
    if (t === 'event') {
      const ch = frame.channel || '';
      if (ch === STATS_CHANNEL) {
        renderStats(frame.data);
        return;
      }
      let payload = frame.data;
      let fromSid = null;
      if (payload !== null && typeof payload === 'object' && !Array.isArray(payload)
          && typeof payload._from === 'string') {
        fromSid = payload._from;
        payload = { ...payload };
        delete payload._from;
      }
      const data = typeof payload === 'string' ? payload : JSON.stringify(payload);
      const id = frame.id ? ` id=${frame.id}` : '';
      logFrame('event', ch, `${data}${id}`, fromSid);
      return;
    }
    // Unknown frame &mdash; surface for debugging.
    logFrame('system', '', JSON.stringify(frame));
  }

  // ---------------- stats ----------------
  function renderStats(data) {
    if (!data) return;
    let snap = data;
    if (typeof snap === 'string') {
      try { snap = JSON.parse(snap); } catch (_) { /* leave as string */ }
    }
    if (typeof snap === 'object' && snap !== null) {
      els.statsGrid.style.display = '';
      els.statsGrid.querySelectorAll('[data-stat]').forEach((node) => {
        const key = node.dataset.stat;
        if (key in snap) node.textContent = formatStat(snap[key]);
      });
      els.statsRaw.textContent = JSON.stringify(snap, null, 2);
    } else {
      els.statsRaw.textContent = String(snap);
    }
    lastStatsAt = Date.now();
    updateStatsAge();
  }

  function formatStat(v) {
    if (typeof v === 'number') return v.toLocaleString();
    return String(v);
  }

  function updateStatsAge() {
    if (!lastStatsAt) {
      els.statsAge.textContent = '—';
      return;
    }
    const dt = Math.floor((Date.now() - lastStatsAt) / 1000);
    els.statsAge.textContent = `${dt}s ago`;
  }
  setInterval(updateStatsAge, 1000);

  // ---------------- subscribe / publish ----------------
  function subscribe() {
    const ch = els.channelName.value.trim();
    if (!ch) return;
    send({ type: 'subscribe', channel: ch });
  }
  function publish() {
    const ch = els.channelName.value.trim();
    if (!ch) return;
    let data;
    const raw = els.msgBody.value.trim();
    try { data = JSON.parse(raw); }
    catch (_) { data = raw; }
    // The protocol's event frame carries no publisher identity
    // (docs/PROTOCOL.md), so provenance has to ride in the client-authored
    // payload. Objects only: never silently promote a user's string or
    // number payload into an object. Any client can put any value here,
    // so the receiving chip is a display convenience, not an identity claim.
    // Skip the stats channel: renderStats consumes its frame before the
    // generic event path strips _from, so stamping it there would surface
    // the key in the raw snapshot pane.
    if (ch !== STATS_CHANNEL && socketId && data !== null
        && typeof data === 'object' && !Array.isArray(data)) {
      data = { ...data, _from: socketId };
    }
    send({ type: 'publish', channel: ch, data });
  }

  // ---------------- stress test ----------------
  async function runStress() {
    if (stressActive) return;
    const key = readKey();
    if (!key) { logFrame('error', '', 'stress: need an api key'); return; }
    stressActive = true;
    els.btnStress.disabled = true;
    let opened = 0, closed = 0;
    const updateLabel = () => {
      els.stressStatus.textContent = `phantom: opened ${opened}/${STRESS_CONNS}, closed ${closed}`;
    };
    updateLabel();
    const url = `${wsBase}?key=${encodeURIComponent(key)}`;
    const sockets = [];
    for (let i = 0; i < STRESS_CONNS; i++) {
      try {
        const s = new WebSocket(url);
        s.onopen = () => { opened++; updateLabel(); };
        s.onclose = () => { closed++; updateLabel(); };
        s.onerror = () => { /* swallow &mdash; counted by close */ };
        sockets.push(s);
      } catch (e) {
        logFrame('error', '', `stress dial: ${e}`);
      }
      // Tiny stagger so we don't slam the loop.
      if (i % 10 === 9) await new Promise((r) => setTimeout(r, 30));
    }
    // Hold for STRESS_HOLD_MS, then close all.
    setTimeout(() => {
      sockets.forEach((s) => {
        try { s.close(1000, 'stress-end'); } catch (_) { /* ignore */ }
      });
      stressActive = false;
      els.btnStress.disabled = !(ws && ws.readyState === WebSocket.OPEN);
      els.stressStatus.textContent = `done: opened ${opened}, closed ${closed}`;
    }, STRESS_HOLD_MS);
  }

  // ---------------- wiring ----------------
  els.btnConnect.addEventListener('click', connect);
  els.btnDisconnect.addEventListener('click', disconnect);
  els.btnSubscribe.addEventListener('click', subscribe);
  els.btnPublish.addEventListener('click', publish);
  els.btnStress.addEventListener('click', runStress);

  // Pre-fill key from URL if present.
  const initial = new URLSearchParams(location.search).get('key');
  if (initial) els.apiKey.value = initial;

  setStatus('idle', 'idle');
  refreshSubList();
})();
