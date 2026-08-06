// wirefan demo client. vanilla JS, no build step.
// Three panels: connection / messages / stats. Plus a stress button.

(() => {
  'use strict';

  const STATS_CHANNEL = '_wirefan-stats';
  const STRESS_CONNS = 50;
  const STRESS_HOLD_MS = 10_000;

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
    statsRaw: $('statsRaw'),
    statsAge: $('statsAge'),
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
    }
  }

  // Build a log row using DOM APIs (no innerHTML &mdash; avoids XSS via frame data).
  function logFrame(kind, channel, body) {
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

    bodyEl.appendChild(document.createTextNode(body));

    row.appendChild(tsEl);
    row.appendChild(bodyEl);

    // Newest at top.
    els.log.prepend(row);
    while (els.log.children.length > 200) {
      els.log.removeChild(els.log.lastChild);
    }
    messageCount++;
    els.msgCount.textContent = `${messageCount} received`;
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
      // server rejects it as reserved, note it quietly instead of
      // alarming the demo log.
      if (frame.code === 'RESERVED_CHANNEL' && statsSubPending) {
        statsSubPending = false;
        els.statsRaw.textContent = 'stats channel not open to clients on this server build';
        logFrame('system', STATS_CHANNEL, 'stats subscribe rejected (reserved channel)');
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
      const data = typeof frame.data === 'string'
        ? frame.data
        : JSON.stringify(frame.data);
      const id = frame.id ? ` id=${frame.id}` : '';
      logFrame('event', ch, `${data}${id}`);
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
