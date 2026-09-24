// wirefan live demo: page logic.
//
// Dogfoods the packaged client library (clients/js). Its built ESM output is
// vendored into web/wirefan-client.js (one dependency-free file) so the Go
// binary can embed it with no bundler or build step; regenerate it with
// `cd clients/js && npm run vendor:web`, never by hand. This file is demo glue
// only: the connection lifecycle, the live diagram (diagram.js), the stats
// strip, the arrivals feed and the "under the hood" developer panel.
//
// Divergence from the design spec (feature 11, "three panels: connection,
// messages, live stats"): all three panels still exist, as tabs under
// "Under the hood", and the page now leads with a live fanout diagram so a
// visitor who is not an engineer sees what fanout means in the first
// seconds. The capped stress button (50 sockets, 10 s) is kept but only
// shown with &dev=1: on the public, shared demo key it raised the connection
// count for every visitor.
//
// Every visitor shares the demo key and the "demo" channel, so the first
// screen renders only what it can validate: pulses ({kind:"pulse", n, _from})
// are drawn, and any other payload is listed as "Custom message from ·XXXX"
// with its text kept to the raw frames under the hood.
import {
  WirefanClient, WirefanError, AckTimeoutError, ConnectionClosedError,
} from './wirefan-client.js';
import { FanoutDiagram } from './diagram.js';

const STATS_CHANNEL = '_wirefan-stats';
const DEFAULT_CHANNEL = 'demo';

// Publish budget for this tab. The demo key is shared by every visitor
// (100 publishes/s across all of them, docs/PROTOCOL.md section 9), so a tab
// never sends more than 4 in any 1 s window (pulses and custom payloads
// together), and nothing is ever sent on a timer: every publish on this page
// starts with a click.
const PULSE_MAX = 4;
const PULSE_WINDOW_MS = 1000;

const MAX_ATTEMPTS = 8;             // reconnect attempts before giving up
// Refusals worth retrying on the initial subscribes: a crowd on the shared
// key can briefly exhaust its budget. Anything else is a real answer.
const TRANSIENT_CODES = new Set(['RATE_LIMITED', 'RATE_LIMITED_CONN', 'SUBSCRIBE_FAILED']);
const STRESS_CONNS = 50;
const STRESS_HOLD_MS = 10_000;
const FEED_MAX = 5;
const FRAMES_MAX = 200;
const CODE_PUBLISHES_MAX = 6;
const RATE_WINDOW_MS = 5_000;
const ANNOUNCE_GAP_MS = 4_000;
const HEARD_TTL_MS = 10 * 60_000;   // forget a tab that has not pulsed for this long
const FRESH_MS = 6_000;             // a pulse this recent outranks the stats count
const BC_NAME = 'wirefan-demo';
const BC_BEAT_MS = 20_000;
const BC_TTL_MS = 75_000;
const STATS_STALE_MS = 12_000;
const SPARK_POINTS = 24;            // 2 minutes of 5 s snapshots
const RTT_KEEP = 20;
const ULID_RE = /^[0-9A-HJKMNP-TV-Z]{26}$/;
const HOOD_KEY = 'wirefan-demo:hood-open';
const TAB_KEY = 'wirefan-demo:hood-tab';
const README_URL = 'https://github.com/EthanY33/wirefan'; // its header links the current demo

// ---------------- DOM ----------------
const $ = (id) => document.getElementById(id);
const els = {
  status: $('status'), statusText: $('statusText'),
  eyebrow: $('eyebrow'), eyebrowText: $('eyebrowText'),
  stage: $('stage'), wire: $('wire'), stageChannel: $('stageChannel'), stageCount: $('stageCount'),
  stageLast: $('stageLast'), legendChannel: $('legendChannel'), legendCounted: $('legendCounted'),
  overlay: $('stageOverlay'), overlayPanel: $('overlayPanel'),
  overlayKicker: $('overlayKicker'), overlayKickerText: $('overlayKickerText'),
  overlayTitle: $('overlayTitle'), overlayBody: $('overlayBody'), overlayActions: $('overlayActions'),
  overlayForm: $('overlayForm'), overlayKey: $('overlayKey'),
  btnPulse: $('btnPulse'), share: $('share'), btnTab: $('btnTab'), btnCopy: $('btnCopy'), copyLabel: $('copyLabel'),
  steps: $('steps'), step1Label: $('step1Label'), hint: $('hint'), notice: $('notice'),
  statConns: $('statConns'), statRate: $('statRate'), statRecv: $('statRecv'), statRtt: $('statRtt'),
  statsSay: $('statsSay'), numbers: $('numbers'), feedSection: $('feedSection'),
  feed: $('feed'), announcer: $('announcer'),
  hood: $('hood'),
  kvState: $('kvState'), kvSocket: $('kvSocket'), kvEndpoint: $('kvEndpoint'), kvProto: $('kvProto'),
  kvSubs: $('kvSubs'), kvReconnects: $('kvReconnects'), kvRtt: $('kvRtt'), kvClose: $('kvClose'),
  keyForm: $('keyForm'), apiKey: $('apiKey'), btnConnect: $('btnConnect'), btnDisconnect: $('btnDisconnect'),
  frames: $('frames'), framesCount: $('framesCount'), showStats: $('showStats'), btnClearFrames: $('btnClearFrames'),
  pubForm: $('pubForm'), pubBody: $('pubBody'), pubChannel: $('pubChannel'), btnPublish: $('btnPublish'),
  statsGrid: $('statsGrid'), statsCaption: $('statsCaption'), statsRaw: $('statsRaw'), statsAge: $('statsAge'),
  serverRate: $('serverRate'), sparkSvg: $('sparkSvg'), sparkEmpty: $('sparkEmpty'),
  codePane: $('codePane'), codeBody: $('codeBody'), btnCopyCode: $('btnCopyCode'),
  stressRow: $('stressRow'), btnStress: $('btnStress'), stressStatus: $('stressStatus'),
};

// ---------------- config from the URL ----------------
const params = new URLSearchParams(location.search);
const CHANNEL = pickChannel(params.get('channel'));
const IS_DEFAULT_CHANNEL = CHANNEL === DEFAULT_CHANNEL;
const DEV = params.get('dev') === '1';
let activeKey = (params.get('key') || '').trim();

// Public channels only: `_` names are reserved for the server, and
// private-/presence- channels need a token minted by an app server, which a
// static demo page does not have.
function pickChannel(raw) {
  if (!raw) return DEFAULT_CHANNEL;
  if (raw.startsWith('_') || raw.startsWith('private-') || raw.startsWith('presence-')) return DEFAULT_CHANNEL;
  if (/[\u0000-\u001f\u007f]/.test(raw)) return DEFAULT_CHANNEL;
  if (new TextEncoder().encode(raw).length > 128) return DEFAULT_CHANNEL;
  return raw;
}

// ---------------- state ----------------
let client = null;            // WirefanClient while connecting, live or reconnecting
let connState = 'idle';       // idle | connecting | live | reconnecting | closed | error | badkey
let connDetail = null;
let dialHint = null;          // what the /v1/connect probe learned about a failing first dial
let mySid = null;
let demoSubscribed = false;
let statsSubscribed = false;
let statsState = 'waiting';   // waiting | ok | refused | failed
let joining = false;
let statsJoining = false;
let statsRetryTimer = null;
let reconnects = 0;
let userClosed = false;
let connections = null;       // from the latest _wirefan-stats snapshot
let lastSnap = null;
let lastStatsAt = 0;
let prevPublished = null;     // { v, t } for the server-wide publish rate
const rateSamples = [];       // [{ rate, at }]
let received = 0;
const recvTimes = [];
let pulseSeq = 0;
const ownPending = new Map(); // pulse n -> { t0, hubAt, peers }
const rtts = [];
// Other tabs on this channel: sid -> { bc, heard, bcSince } (Date.now() of
// the last BroadcastChannel word from it, of the last pulse it sent here, and
// of the first BroadcastChannel word in its current run).
const peers = new Map();
let peerView = { list: [], peers: 0, counted: 0 };
const progress = { peer: false, fanout: false };
let soloPulses = 0;
let stressActive = false;
let overlayTimer = null;
let overlayFormOpen = false;
const BASE_TITLE = document.title;

const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
const coarsePointer = window.matchMedia('(pointer: coarse)');
const canShare = typeof navigator.share === 'function';
const diagram = new FanoutDiagram(els.wire, { reducedMotion });

// ---------------- helpers ----------------
const short = (sid) => (sid ? sid.slice(-4) : '');
const pad2 = (n) => String(n).padStart(2, '0');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const q = (s) => JSON.stringify(s);
function clock() {
  const d = new Date();
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}
function node(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}
function plural(n, word) { return `${n.toLocaleString()} ${word}${n === 1 ? '' : 's'}`; }
function describe(e) {
  if (e instanceof WirefanError) return e.code ? `${e.code}: ${e.message}` : e.message;
  return String((e && e.message) || e);
}
const ms = (v) => Math.max(1, Math.round(v));

// Client-side sliding window for publishes (see PULSE_MAX above). A token
// bucket with a burst would let up to 8 through in one second; a window of
// send times keeps "4 per second" literally true.
const sentAt = [];            // performance.now() of this tab's recent publishes, oldest first
function pruneSent(now) {
  while (sentAt.length && now - sentAt[0] >= PULSE_WINDOW_MS) sentAt.shift();
}
function takeToken() {
  const now = performance.now();
  pruneSent(now);
  if (sentAt.length >= PULSE_MAX) return false;
  sentAt.push(now);
  return true;
}

let noticeTimer = null;
function showNotice(text, tone = 'warn', holdMs = 4000) {
  els.notice.dataset.tone = tone;
  els.notice.textContent = text;
  els.hint.classList.add('is-covered'); // the notice takes the hint's slot, so nothing shifts
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => {
    els.notice.textContent = '';
    els.hint.classList.remove('is-covered');
  }, holdMs);
}

// Screen readers hear arrivals in batches, never one announcement per event.
let announceQueue = { count: 0, last: '' };
let announceTimer = null;
function announce(text) {
  announceQueue.count += 1;
  announceQueue.last = text;
  if (!announceTimer) announceTimer = setTimeout(flushAnnounce, 1200);
}
function flushAnnounce() {
  const { count, last } = announceQueue;
  announceQueue = { count: 0, last: '' };
  if (!count) { announceTimer = null; return; }
  els.announcer.textContent = count === 1 ? last : `${count} messages arrived. Latest: ${last}`;
  announceTimer = setTimeout(flushAnnounce, ANNOUNCE_GAP_MS);
}

// ---------------- code transcript ----------------
// Each block is the @wirefan/client call the page just made, highlighted by
// a tiny tokenizer. Everything goes through textContent, never innerHTML.
const TOKEN = /(\/\/.*$)|("(?:[^"\\]|\\.)*")|\b(import|from|const|new|await)\b|\b(\d+(?:\.\d+)?)\b|([A-Za-z_$][\w$]*)(?=\()/g;
function highlight(line) {
  const frag = document.createDocumentFragment();
  let last = 0;
  TOKEN.lastIndex = 0;
  let m;
  while ((m = TOKEN.exec(line))) {
    if (m.index > last) frag.append(line.slice(last, m.index));
    const cls = m[1] ? 'tok-com' : m[2] ? 'tok-str' : m[3] ? 'tok-kw' : m[4] ? 'tok-num' : 'tok-fn';
    frag.append(node('span', cls, m[0]));
    last = m.index + m[0].length;
  }
  if (last < line.length) frag.append(line.slice(last));
  return frag;
}
let codeTrimmed = 0;
function code(lines, kind = 'misc') {
  const pane = els.codePane;
  const nearBottom = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 40;
  const block = node('span', 'code-block');
  block.dataset.kind = kind;
  lines.forEach((line, i) => {
    if (i > 0) block.append('\n');
    block.append(highlight(line));
  });
  block.append('\n');
  els.codeBody.append(block);
  const pubs = els.codeBody.querySelectorAll('.code-block[data-kind="publish"]');
  const extra = pubs.length - CODE_PUBLISHES_MAX;
  if (extra > 0) {
    for (let i = 0; i < extra; i++) pubs[i].remove();
    codeTrimmed += extra;
    let note = els.codeBody.querySelector('.code-block[data-kind="trimmed"]');
    if (!note) {
      note = node('span', 'code-block');
      note.dataset.kind = 'trimmed';
      els.codeBody.querySelector('.code-block[data-kind="publish"]').before(note);
    }
    note.replaceChildren(highlight(`// ${codeTrimmed} earlier publish${codeTrimmed === 1 ? '' : 'es'} not shown`), '\n');
  }
  if (nearBottom) pane.scrollTop = pane.scrollHeight;
}

// ---------------- raw frame tap ----------------
// A WebSocket that copies every frame into the raw-frames panel. The client
// library takes an injected implementation (clients/js README, "Node /
// injection"); this one changes nothing about what goes over the wire.
class TapSocket extends WebSocket {
  constructor(url, protocols) {
    super(url, protocols);
    this.addEventListener('open', () => tapNote('open', `socket open ${String(url)}`));
    this.addEventListener('message', (ev) => {
      if (typeof ev.data === 'string') tapFrame('in', ev.data);
    });
    this.addEventListener('close', (ev) => {
      const text = `code ${ev.code}${ev.reason ? ` "${ev.reason}"` : ''}${ev.wasClean ? '' : ' (not clean)'}`;
      els.kvClose.textContent = `${text} at ${clock()}`;
      tapNote('close', text);
    });
  }
  send(data) {
    if (typeof data === 'string') tapFrame('out', data);
    super.send(data);
  }
}

const utf8 = new TextEncoder();
let framesEmpty = true;
let framesShown = 0;
function frameRow(dir, label) {
  if (framesEmpty) { els.frames.replaceChildren(); framesEmpty = false; }
  const li = node('li');
  li.append(node('span', 'ts', clock()));
  li.append(node('span', `dir dir-${dir}`, label));
  return li;
}
function pushFrame(li) {
  els.frames.prepend(li);
  while (els.frames.children.length > FRAMES_MAX) els.frames.lastElementChild.remove();
}
function tapFrame(dir, text) {
  let msg = null;
  try { msg = JSON.parse(text); } catch (_) { /* shown verbatim below */ }
  const type = msg && typeof msg.type === 'string' ? msg.type : '?';
  if (type === 'connected' && typeof msg.version === 'string') els.kvProto.textContent = msg.version;
  const li = frameRow(dir, dir === 'in' ? 'IN' : 'OUT');
  li.dataset.kind = type;
  const stats = msg && msg.channel === STATS_CHANNEL;
  if (stats) li.dataset.stats = '';
  const body = node('span', 'body');
  body.append(node('span', 'ty', type), node('span', 'size', `${utf8.encode(text).length} B`), ' ', text);
  li.append(body);
  pushFrame(li);
  if (!stats) {
    framesShown += 1;
    els.framesCount.textContent = framesShown.toLocaleString();
  }
}
function tapNote(kind, text) {
  const li = frameRow(kind, kind.toUpperCase());
  li.dataset.kind = kind;
  li.append(node('span', 'body', text));
  pushFrame(li);
}

// ---------------- connection ----------------
function endpointFor(key) {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  return `${scheme}://${location.host}/v1/connect?key=${encodeURIComponent(key)}`;
}

async function connect(key) {
  if (!key) { setConnState('idle'); return; }
  if (client) dropClient();
  userClosed = false;
  activeKey = key;
  dialHint = null;
  statsState = 'waiting';
  clearTimeout(statsRetryTimer);
  let everConnected = false;
  const c = new WirefanClient({
    url: location.origin,
    key,
    // Bounded retries so a server that stays down ends in a clear
    // "could not reach" state instead of retrying forever.
    reconnect: { maxAttempts: MAX_ATTEMPTS },
    webSocket: TapSocket,
  });
  client = c;
  els.kvEndpoint.textContent = endpointFor(key);
  code([
    'import { WirefanClient } from "@wirefan/client";',
    '',
    `const client = new WirefanClient({ url: ${q(location.origin)}, key: ${q(key)} });`,
  ], 'setup');
  setConnState('connecting');

  c.on('connected', ({ socketId, reconnected }) => {
    if (client !== c) return;
    everConnected = true;
    dialHint = null;
    mySid = socketId;
    els.kvSocket.textContent = socketId;
    diagram.setMe(short(socketId));
    document.title = `wirefan · ${short(socketId)}`;
    if (reconnected) {
      reconnects += 1;
      els.kvReconnects.textContent = String(reconnects);
      // Tabs placed only by their pulses may have left while this one was
      // away; they come back as soon as they pulse again. Tabs in this
      // browser re-announce themselves over BroadcastChannel.
      for (const [sid, p] of peers) if (!p.bc) peers.delete(sid);
      // The last snapshot predates the drop (the server may even have
      // restarted), so count again from the next one.
      connections = null;
      prevPublished = null;
      demoSubscribed = false;
      statsSubscribed = false;
      renderSubs();
      code([`// reconnected as socket_id ${q(socketId)}; the client resubscribes on its own`], 'event');
      // The client restores the subscriptions and reports it with
      // "resubscribed". If it could not (a definitive refusal drops the
      // channel), join again once the dust settles.
      setConnState('connecting', { phase: 'restoring' });
      setTimeout(() => {
        if (client === c && c.state === 'connected') joinChannels(c);
      }, 4000);
    } else {
      code([`await client.connect(); // socket_id ${q(socketId)}`], 'connect');
    }
  });
  c.on('resubscribed', ({ channels }) => {
    if (client !== c) return;
    if (channels.includes(CHANNEL)) demoSubscribed = true;
    if (channels.includes(STATS_CHANNEL)) statsSubscribed = true;
    renderSubs();
    if (demoSubscribed && c.state === 'connected' && connState !== 'live') {
      setConnState('live');
      bcHello();
    }
  });
  c.on('reconnecting', ({ attempt, delayMs }) => {
    if (client !== c) return;
    if (!everConnected) {
      // The first dial failed. A WebSocket handshake that fails shows the
      // page nothing but a close, so ask the server once why (see
      // diagnoseFirstFailure) while the client keeps retrying.
      setConnState('connecting', { phase: 'dial', attempt });
      if (attempt === 1) diagnoseFirstFailure(c, key);
      return;
    }
    setConnState('reconnecting', { attempt, delayMs });
  });
  c.on('disconnected', ({ willReconnect, code: closeCode }) => {
    if (client !== c) return;
    bcPost('bye');
    demoSubscribed = false;
    if (!willReconnect || !everConnected) return;
    // The last snapshot described a connection that is gone; the count
    // starts again from the first snapshot after the reconnect.
    connections = null;
    prevPublished = null;
    code([`// connection dropped (close ${closeCode ?? 'code unknown'}); the client reconnects with backoff`], 'event');
    setConnState('reconnecting', { attempt: 0 });
  });
  c.on('error', (err) => { if (client === c) onClientError(err); });
  c.on('closed', ({ reason }) => {
    if (client !== c) return;
    client = null;
    resetSession();
    setConnState(reason === 'exhausted' ? 'closed' : 'idle');
  });

  try {
    await c.connect();
  } catch (_) {
    // "closed" or diagnoseFirstFailure already told the visitor what happened.
    return;
  }
  if (client !== c) return;
  await joinChannels(c);
}

// Join the demo channel, then the stats channel. Safe to call again: a
// channel that is already confirmed is skipped, and subscribe() on one the
// library is still restoring joins that attempt instead of sending another.
async function joinChannels(c) {
  if (joining) return;
  joining = true;
  try {
    if (!demoSubscribed) {
      const ok = await subscribeWithRetry(c, CHANNEL, onChannelEvent, (attempt) => {
        if (client === c && connState !== 'live') setConnState('connecting', { phase: 'join', attempt });
      });
      if (!ok) return;
      demoSubscribed = true;
      code([`await client.subscribe(${q(CHANNEL)}, onEvent);`], 'subscribe');
      renderSubs();
      setConnState('live');
      bcHello();
    }
  } catch (e) {
    if (client === c) setConnState('error', { message: describe(e) });
    return;
  } finally {
    joining = false;
  }
  joinStats(c);
}

// The stats system channel feeds the connection count. Automatic
// resubscription restores it after reconnects; this is the first join, and
// a retry if it could not be restored.
async function joinStats(c) {
  if (statsSubscribed || statsJoining || statsState === 'refused') return;
  statsJoining = true;
  try {
    const ok = await subscribeWithRetry(c, STATS_CHANNEL, onStats);
    if (ok) {
      statsSubscribed = true;
      if (statsState !== 'ok') statsState = 'waiting';
      code([`await client.subscribe(${q(STATS_CHANNEL)}, onStats); // read-only system channel`], 'subscribe');
      renderSubs();
    }
  } catch (e) {
    if (client !== c) return;
    if (e instanceof WirefanError && e.code === 'RESERVED_CHANNEL') {
      statsState = 'refused';
      els.statsCaption.textContent = 'This server build keeps the _wirefan-stats channel closed to clients, so the connection count is unavailable.';
      els.statsGrid.hidden = true;
      els.statsRaw.textContent = 'stats channel not open to clients on this server build';
    } else {
      // Try once more in a while (a subscribe, never a publish).
      statsState = 'failed';
      clearTimeout(statsRetryTimer);
      statsRetryTimer = setTimeout(() => { if (client === c && c.state === 'connected') joinStats(c); }, 30_000);
    }
    renderStatNumbers();
    refreshPeers();
  } finally {
    statsJoining = false;
  }
}

// Every visitor shares one key's rate limit, and subscribes draw on it too,
// so a crowd arriving at once can briefly refuse one. Retry those refusals
// with jittered exponential backoff; give up (false) quietly if the
// connection goes away, since the reconnect path joins again.
async function subscribeWithRetry(c, channel, handler, onRetry) {
  for (let attempt = 0; ; attempt++) {
    if (client !== c || c.state !== 'connected') return false;
    try {
      await c.subscribe(channel, handler);
      return client === c;
    } catch (e) {
      if (client !== c || e instanceof ConnectionClosedError) return false;
      const retryable = e instanceof AckTimeoutError
        || (e instanceof WirefanError && TRANSIENT_CODES.has(e.code));
      if (!retryable || attempt >= 6) throw e;
      if (onRetry) onRetry(attempt + 1);
      await sleep(Math.min(8000, 500 * 2 ** attempt) * (0.75 + Math.random() * 0.5));
    }
  }
}

// /v1/connect checks the key before anything else, and a plain GET with a
// good key gets 426 (it is not an upgrade), so one GET after a failed first
// dial tells a revoked key (401) from the per-IP cap (429), a restarting
// server (503) or a server that is simply down (no answer).
async function diagnoseFirstFailure(c, key) {
  let status = 0;
  try {
    const res = await fetch(`/v1/connect?key=${encodeURIComponent(key)}`, { cache: 'no-store' });
    status = res.status;
  } catch (_) { /* network down: the reconnect loop keeps trying */ }
  if (client !== c || c.state === 'connected') return;
  if (status === 401) {
    client = null;
    c.close();
    resetSession();
    setConnState('badkey');
    return;
  }
  if (status === 429) dialHint = 'ipcap';
  else if (status === 503) dialHint = 'draining';
  if (dialHint) renderOverlay();
}

// Explicit close: the library never reconnects after this.
function dropClient() {
  const c = client;
  client = null;
  bcPost('bye');
  if (c) {
    c.close();
    code(['client.close();'], 'close');
  }
  resetSession();
}

function disconnect() {
  if (!client) return;
  userClosed = true;
  dropClient();
  setConnState('idle');
}

function resetSession() {
  mySid = null;
  demoSubscribed = false;
  statsSubscribed = false;
  if (statsState !== 'refused') statsState = 'waiting';
  clearTimeout(statsRetryTimer);
  connections = null;
  prevPublished = null;
  ownPending.clear();
  els.kvSocket.textContent = '–';
  document.title = BASE_TITLE;
  diagram.setMe('');
  renderSubs();
  renderStatNumbers();
  refreshPeers();
}

function renderSubs() {
  const subs = [];
  if (demoSubscribed) subs.push(CHANNEL);
  if (statsSubscribed) subs.push(STATS_CHANNEL);
  els.kvSubs.textContent = subs.length ? subs.join(', ') : '–';
}

function onClientError(err) {
  const code = err instanceof WirefanError ? err.code : '';
  const op = err instanceof WirefanError ? err.op : undefined;
  if (code === 'RATE_LIMITED' || code === 'RATE_LIMITED_CONN') {
    if (!op || op === 'publish') refusePending();
    showNotice(code === 'RATE_LIMITED'
      ? 'Everyone here shares this demo key and it just hit its rate limit, so the server refused that one. Try again in a moment.'
      : 'This tab reached its per-connection limit. Try again in a moment.', 'warn');
    return;
  }
  if (code === 'NOT_SUBSCRIBED') {
    refusePending();
    demoSubscribed = false;
    showNotice('Rejoining the channel. Try again in a moment.', 'warn');
    if (client) joinChannels(client);
    return;
  }
  showNotice(`The server said: ${describe(err)}`, 'err', 6000);
}

// A refused publish never comes back: drop the oldest pending pulse and let
// the hub show the refusal.
function refusePending() {
  const first = ownPending.keys().next();
  if (!first.done) ownPending.delete(first.value);
  diagram.refuse();
}

// ---------------- connection state -> UI ----------------
const STATUS = {
  idle: ['idle', 'Offline'], connecting: ['connecting', 'Connecting'], live: ['live', 'Live'],
  reconnecting: ['reconnecting', 'Reconnecting'], closed: ['closed', 'Offline'],
  error: ['error', 'Error'], badkey: ['error', 'Key rejected'],
};
function setConnState(state, detail = null) {
  const changed = state !== connState;
  connState = state;
  connDetail = detail;
  if (changed) overlayFormOpen = false;
  const [pill, label] = STATUS[state];
  els.status.dataset.state = pill;
  // role=status: rewrite only on a real change, so screen readers do not
  // hear "Reconnecting" again on every attempt.
  if (els.statusText.textContent !== label) els.statusText.textContent = label;
  let kv = state;
  if (detail && detail.phase) kv += ` (${detail.phase}${detail.attempt ? `, attempt ${detail.attempt}` : ''})`;
  else if (detail && detail.attempt) kv += ` (attempt ${detail.attempt} of ${MAX_ATTEMPTS}, in ${detail.delayMs} ms)`;
  els.kvState.textContent = kv;
  els.stage.classList.toggle('is-live', state === 'live');
  diagram.setOffline(state !== 'live');

  const live = state === 'live';
  els.btnPulse.disabled = !live;
  els.btnPublish.disabled = !live;
  els.btnStress.disabled = !live || stressActive;
  els.btnDisconnect.disabled = !client;
  els.btnConnect.textContent = client ? 'Reconnect with this key' : 'Connect';

  renderChrome();
  renderOverlay();
  renderStatNumbers();
  refreshPeers(); // also re-renders the hint, the steps and the stage count
}

// The eyebrow, the share buttons and the steps follow the key: a link with
// no key, or a key the server refused, has nothing worth sharing.
function renderChrome() {
  const usable = !!activeKey && connState !== 'badkey';
  els.share.hidden = !usable;
  els.steps.hidden = !usable;
  // Numbers that can never move without a key would only read as broken.
  els.numbers.hidden = !usable;
  els.feedSection.hidden = !usable;
  let text = 'Live demo on a real server';
  let tone = 'live';
  if (!activeKey) { text = 'Offline: no key in this link'; tone = 'off'; }
  else if (connState === 'badkey') { text = 'Offline: key not accepted'; tone = 'off'; }
  else if (connState === 'closed') { text = 'Offline: server not answering'; tone = 'off'; }
  else if (connState === 'error') { text = 'Offline: could not join'; tone = 'off'; }
  else if (connState === 'idle') { text = userClosed ? 'Offline: disconnected' : 'Offline'; tone = 'off'; }
  else if (connState === 'reconnecting') { text = 'Connection lost: reconnecting'; tone = 'wait'; }
  else if (serverSilent()) { text = 'Waiting for the server'; tone = 'wait'; }
  els.eyebrowText.textContent = text;
  els.eyebrow.dataset.tone = tone;
}

function overlayButton(label, onClick, kind = 'secondary') {
  const b = node('button', `btn btn-${kind} btn-sm`, label);
  b.type = 'button';
  b.addEventListener('click', onClick);
  return b;
}
function overlayLink(label, href) {
  const a = node('a', 'btn btn-ghost btn-sm', label);
  a.href = href;
  a.target = '_blank';
  a.rel = 'noopener';
  return a;
}
function openOverlayForm() {
  overlayFormOpen = true;
  renderOverlay();
  els.overlayKey.focus();
}

function renderOverlay() {
  clearTimeout(overlayTimer);
  let kicker = 'Offline';
  let tone = 'off';
  let title = '';
  let body = '';
  let actions = [];
  let form = false;
  const d = connDetail || {};
  const ch = `#${CHANNEL}`;
  switch (connState) {
    case 'live': {
      // A keyboard user who connected from the panel (a pasted key,
      // Reconnect, Try again) would land on <body> when it disappears; hand
      // their place to the button they came for.
      const hadFocus = els.overlay.contains(document.activeElement);
      els.overlay.hidden = true;
      els.stage.classList.remove('has-panel');
      if (hadFocus) els.btnPulse.focus({ preventScroll: true });
      return;
    }
    case 'idle':
      if (!activeKey) {
        kicker = 'No key';
        title = 'No demo key in this link';
        body = 'Links to this demo carry one as ?key=. Running wirefan yourself? Paste a key id from POST /v1/keys.';
        form = overlayFormOpen;
        actions = form ? [] : [overlayButton('Paste a key id', openOverlayForm), overlayLink('Find the demo link', README_URL)];
      } else {
        title = userClosed ? 'Disconnected' : 'Offline';
        body = userClosed ? 'You closed the connection. Nothing is sent or received until you reconnect.' : '';
        actions = [overlayButton('Reconnect', () => connect(activeKey), 'primary')];
      }
      break;
    case 'badkey':
      kicker = 'Key rejected';
      tone = 'err';
      title = "This demo link's key is no longer valid";
      body = `The server did not accept key ·${short(activeKey)}. It was probably rotated after this link was shared; the project README always links the current demo.`;
      form = overlayFormOpen;
      actions = form ? [] : [overlayLink('Open the README', README_URL), overlayButton('Paste a key id', openOverlayForm)];
      break;
    case 'connecting':
      kicker = 'Connecting';
      tone = 'wait';
      title = 'Connecting';
      body = 'Opening a WebSocket to the server.';
      if (d.phase === 'dial' && dialHint === 'ipcap') {
        title = 'Too many connections from your network';
        body = 'The server caps open connections per address. Close a few demo tabs and this one will connect.';
      } else if (d.phase === 'dial' && dialHint === 'draining') {
        title = 'The server is restarting';
        body = 'It should be back in a few seconds. This tab keeps trying.';
      } else if (d.phase === 'dial') {
        body = `The server has not answered yet. Trying again (attempt ${d.attempt} of ${MAX_ATTEMPTS}).`;
      } else if (d.phase === 'join') {
        title = `Joining ${ch}`;
        body = `Every visitor shares one demo key and it is busy right now, so this tab is retrying (attempt ${d.attempt}).`;
      } else if (d.phase === 'restoring') {
        kicker = 'Reconnected';
        title = 'Back online';
        body = `Rejoining ${ch}.`;
      }
      break;
    case 'reconnecting':
      kicker = 'Reconnecting';
      tone = 'wait';
      title = 'Connection lost';
      body = d.attempt
        ? `Reconnecting (attempt ${d.attempt} of ${MAX_ATTEMPTS}). Messages sent while this tab is away are not replayed.`
        : 'Reconnecting. Messages sent while this tab is away are not replayed.';
      break;
    case 'closed':
      tone = 'err';
      title = 'Could not reach the server';
      body = `Gave up after ${MAX_ATTEMPTS} attempts. The server may be restarting.`;
      actions = [overlayButton('Try again', () => connect(activeKey), 'primary')];
      break;
    case 'error':
      tone = 'err';
      kicker = 'Error';
      title = `Could not join ${ch}`;
      body = d.message || '';
      actions = [overlayButton('Try again', () => connect(activeKey), 'primary')];
      break;
    default:
      break;
  }
  const paint = () => {
    // Repainting removes the old buttons and may hide the key form; if focus
    // was on one of them, keep it on the panel instead of dropping to <body>.
    const hadFocus = !els.overlay.hidden && els.overlay.contains(document.activeElement);
    els.overlayKicker.dataset.tone = tone;
    els.overlayKickerText.textContent = kicker;
    els.overlayTitle.textContent = title;
    els.overlayBody.textContent = body;
    els.overlayActions.replaceChildren(...actions);
    els.overlayActions.hidden = actions.length === 0;
    els.overlayForm.hidden = !form;
    els.overlay.hidden = false;
    els.stage.classList.add('has-panel');
    if (hadFocus) {
      const a = document.activeElement;
      const kept = a && els.overlay.contains(a) && a.isConnected && !a.closest('[hidden]');
      if (!kept) els.overlayPanel.focus({ preventScroll: true });
    }
  };
  els.stage.classList.toggle('has-panel', !els.overlay.hidden);
  // A healthy connect takes a few milliseconds; only show the "Connecting"
  // veil if it is still going after a beat, so a normal load never flashes.
  const quiet = !d.phase || d.phase === 'restoring';
  if (connState === 'connecting' && els.overlay.hidden && quiet) overlayTimer = setTimeout(paint, 700);
  else paint();
}

// The first dial failed and the client is still retrying: no server has
// answered this tab yet.
function serverSilent() {
  return connState === 'connecting' && !!connDetail && connDetail.phase === 'dial';
}

function othersCount() {
  return connections === null ? null : Math.max(0, connections - 1);
}
// No count yet, and one is on its way (not refused, not given up on).
function statsPending() {
  return connections === null && statsState !== 'refused' && statsState !== 'failed';
}

function renderStageCount() {
  const el = els.stageCount;
  el.replaceChildren();
  if (connState === 'reconnecting') { el.append('reconnecting'); return; }
  if (connState === 'connecting') { el.append(node('span', 'counting', 'connecting')); return; }
  if (connState !== 'live') { el.append('offline'); return; }
  const { peers: p, counted } = peerView;
  const waiting = statsPending();
  if (waiting && p === 0) { el.append(node('span', 'counting', 'counting connections')); return; }
  el.append(node('b', null, p === 0 ? 'only this tab' : plural(p, 'other tab')));
  if (counted > 0) el.append(node('span', 'note-more', ` + ${counted.toLocaleString()} more on the server`));
  else if (waiting) el.append(node('span', 'note-more counting', ', counting the rest'));
  const { peers: dp, counted: dc } = diagram.drawn;
  if (dp + dc < p + counted) el.append(node('span', 'note-more', `, ${dp + dc} drawn`));
}

function renderHint() {
  let text = '';
  const p = peerView.peers;
  const ch = `#${CHANNEL}`;
  if (connState === 'live') {
    if (progress.fanout && p > 0) {
      text = `That was a fanout: one publish went into the hub, and every tab on ${ch} got its own copy.`;
    } else if (p > 0) {
      text = `Send a pulse from either tab. The hub gets it once and hands a copy to every tab on ${ch}.`;
    } else if (soloPulses > 0) {
      text = coarsePointer.matches
        ? 'That pulse went to the server and back. Share the link to another device to watch it land there too.'
        : 'That pulse went to the server and back. Open a second tab to watch it land in both.';
    } else {
      text = coarsePointer.matches
        ? 'Share the link and open it on another device, then send from either one.'
        : 'Open a second tab, put the two side by side, then send from either one.';
    }
    if (!IS_DEFAULT_CHANNEL && !(progress.fanout && p > 0)) text += ` This is a separate room: only tabs on ${ch} receive it.`;
  } else if (connState === 'connecting' || connState === 'reconnecting') {
    text = 'Waiting for the connection. The client library reconnects on its own.';
  } else if (!activeKey) {
    text = 'Nothing is connected yet: this page needs a key id in its link.';
  } else if (connState === 'badkey') {
    text = "Nothing is connected: the server refused this link's key.";
  } else {
    text = 'Offline. Reconnect to send pulses.';
  }
  els.hint.textContent = text;
  els.hint.hidden = !text;
}

function renderSteps() {
  const live = connState === 'live';
  const trying = connState === 'connecting' || connState === 'reconnecting';
  els.step1Label.textContent = live ? 'Connected' : trying ? 'Connecting' : 'Connect';
  const done = [live, progress.peer, progress.fanout];
  let activeGiven = false;
  [...els.steps.children].forEach((li, i) => {
    let st;
    if (done[i]) st = 'done';
    else if (!activeGiven) { st = 'active'; activeGiven = true; }
    else st = 'todo';
    li.dataset.state = st;
    li.querySelector('.step-sr').textContent = st === 'done' ? ' (done)' : st === 'active' ? ' (next)' : ' (not yet)';
  });
  els.steps.dataset.complete = String(done.every(Boolean));
}

// ---------------- peers ----------------
// Tabs this page can place on this channel: other tabs in this browser that
// said so over BroadcastChannel, and tabs whose pulses arrived here (their
// _from). The server's connection count covers every key and channel, so
// the rest of it is drawn as "other connections on the server", dimmed and
// never animated, and only on the shared default channel.
function refreshPeers() {
  const now = Date.now();
  for (const [sid, p] of peers) {
    if (p.bc && now - p.bc > BC_TTL_MS) p.bc = 0;
    if (p.heard && now - p.heard > HEARD_TTL_MS) p.heard = 0;
    if ((!p.bc && !p.heard) || sid === mySid) peers.delete(sid);
  }
  const all = [...peers.entries()];
  const viaBc = all.filter(([, p]) => p.bc);
  const heardOnly = all.filter(([, p]) => !p.bc).sort((a, b) => b[1].heard - a[1].heard);
  const statOthers = othersCount();
  // A tab heard from minutes ago may have left: keep only as many as the
  // server's count can still account for, plus any that pulsed just now
  // (the next 5 s snapshot may not count them yet). Tabs in this browser that
  // appeared after that snapshot are not in its count either, so they do not
  // use up its room.
  const bcNew = bcSinceSnapshot();
  let room = heardOnly.length;
  if (statOthers !== null) {
    const fresh = heardOnly.filter(([, p]) => now - p.heard < FRESH_MS).length;
    room = Math.min(heardOnly.length, Math.max(fresh, statOthers - (viaBc.length - bcNew), 0));
  }
  const on = !!client;
  const list = on
    ? [...viaBc, ...heardOnly.slice(0, room)]
      .sort((a, b) => Math.max(b[1].bc, b[1].heard) - Math.max(a[1].bc, a[1].heard))
      .map(([sid]) => ({ sid, label: `·${short(sid)}` }))
    : [];
  // The same goes for the rest of the count: a new tab in this browser is
  // drawn as a peer without taking a "counted" terminal away.
  const counted = on && IS_DEFAULT_CHANNEL && statOthers !== null
    ? Math.max(0, statOthers - (list.length - bcNew)) : 0;
  peerView = { list, peers: list.length, counted };
  if (list.length > 0) progress.peer = true;
  diagram.setPeers(list, counted);
  els.legendCounted.hidden = counted === 0;
  renderStageCount();
  renderHint();
  renderSteps();
  renderStatNumbers();
}

// BroadcastChannel presence: tabs in the same browser find each other at
// once, with zero publishes (the message never leaves this machine). wirefan
// itself has no presence events (CLAUDE.md, "Deferred").
let bc = null;
try { bc = new BroadcastChannel(BC_NAME); } catch (_) { bc = null; }
function bcPost(type, sid = mySid) {
  if (!bc || !sid) return;
  try { bc.postMessage({ v: 1, type, sid, key: activeKey, channel: CHANNEL }); } catch (_) { /* closed */ }
}
function bcHello() {
  if (connState === 'live' && mySid) bcPost('hello');
}
if (bc) {
  bc.onmessage = (ev) => {
    const m = ev.data;
    if (!m || typeof m !== 'object' || m.v !== 1 || typeof m.sid !== 'string' || !ULID_RE.test(m.sid)) return;
    if (m.sid === mySid) return;
    if (m.type === 'bye') {
      if (peers.delete(m.sid)) refreshPeers();
      return;
    }
    if (m.key !== activeKey || m.channel !== CHANNEL) return;
    if (m.type !== 'hello' && m.type !== 'here') return;
    const p = peers.get(m.sid) || { bc: 0, heard: 0, bcSince: 0 };
    if (!p.bc) p.bcSince = Date.now();
    p.bc = Date.now();
    peers.set(m.sid, p);
    if (m.type === 'hello' && connState === 'live' && mySid) bcPost('here');
    refreshPeers();
  };
  // Local only: a heartbeat so a tab that crashed without "bye" ages out.
  setInterval(() => {
    if (connState === 'live') bcPost('here');
    refreshPeers();
  }, BC_BEAT_MS);
  window.addEventListener('pagehide', () => bcPost('bye'));
  window.addEventListener('pageshow', (e) => { if (e.persisted) bcHello(); });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) bcHello(); });
}

// ---------------- incoming events ----------------
// The only payload this page draws. _from rides in the payload because the
// event frame carries no publisher identity (docs/PROTOCOL.md); any client
// can write anything there, so it is a display hint, not an identity claim.
function parsePulse(data) {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return null;
  if (data.kind !== 'pulse' || !Number.isSafeInteger(data.n) || data.n < 0) return null;
  if (typeof data._from !== 'string' || !ULID_RE.test(data._from)) return null;
  return { n: data.n, from: data._from };
}
function customSender(data) {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return null;
  return typeof data._from === 'string' && ULID_RE.test(data._from) ? data._from : null;
}

function completeFanout() {
  if (progress.fanout) return;
  progress.fanout = true;
  renderSteps();
  renderHint();
}

function onChannelEvent(ev) {
  const now = performance.now();
  received += 1;
  recvTimes.push(now);
  els.statRecv.textContent = received.toLocaleString();

  const pulse = parsePulse(ev.data);
  const from = pulse ? pulse.from : customSender(ev.data);
  const mine = from !== null && from === mySid;
  const id = typeof ev.id === 'string' && ULID_RE.test(ev.id) ? ev.id : '';

  let rtt = null;
  let hubAt = null;
  if (pulse && mine && ownPending.has(pulse.n)) {
    const p = ownPending.get(pulse.n);
    ownPending.delete(pulse.n);
    rtt = now - p.t0;
    hubAt = p.hubAt;
    recordRtt(rtt);
    if (p.peers > 0) completeFanout();
  }
  if (pulse && !mine) {
    // A pulse is sent only by this page, and only once it is subscribed,
    // so its sender is a tab on this channel.
    const p = peers.get(from) || { bc: 0, heard: 0, bcSince: 0 };
    p.heard = Date.now();
    peers.set(from, p);
    refreshPeers();
    completeFanout();
  }
  diagram.deliver({ from: mine ? 'me' : from, hubAt });
  addFeedRow({ mine, from, rtt, pulse: !!pulse, id });

  // The newest event's real protocol facts, in the corner of the diagram.
  let last = `event ${id || '(no id)'}`;
  if (mine) last += rtt !== null ? `, yours, back in ${ms(rtt)} ms` : ', yours';
  else if (from) last += ` from ·${short(from)}`;
  if (!pulse) last += ', custom payload';
  els.stageLast.textContent = last;
}

function addFeedRow({ mine, from, rtt, pulse, id }) {
  const empty = els.feed.querySelector('.feed-empty');
  if (empty) empty.remove();
  const li = node('li', 'is-new');
  li.addEventListener('animationend', () => li.classList.remove('is-new'), { once: true });
  if (id) li.title = `event id ${id}`;
  li.append(node('span', 't', clock()));
  const what = node('span', 'what');
  let spoken;
  if (pulse && mine) {
    what.append('Your pulse came back');
    if (rtt !== null) what.append(' in ', node('span', 'ms', `${ms(rtt)} ms`));
    spoken = rtt !== null ? `Your pulse came back in ${ms(rtt)} milliseconds.` : 'Your pulse came back.';
  } else if (pulse) {
    what.append('Pulse from ', node('span', 'chip', `·${short(from)}`));
    spoken = `Pulse from tab ${short(from)}.`;
  } else {
    // Never the payload itself: anyone can publish here, and the key id is
    // public. The text stays in the raw frames under the hood.
    if (mine) what.append('Your custom message came back');
    else if (from) what.append('Custom message from ', node('span', 'chip', `·${short(from)}`));
    else what.append('Custom message, sender unnamed');
    const more = node('button', 'linkish', 'see Under the hood');
    more.type = 'button';
    more.addEventListener('click', () => openHood('tab-frames'));
    what.append(' ', more);
    spoken = mine ? 'Your custom message came back.' : from ? `Custom message from tab ${short(from)}.` : 'Custom message.';
  }
  li.append(what);
  els.feed.prepend(li);
  while (els.feed.children.length > FEED_MAX) els.feed.lastElementChild.remove();
  announce(spoken);
}

function recordRtt(v) {
  rtts.push(v);
  if (rtts.length > RTT_KEEP) rtts.shift();
  const sorted = [...rtts].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  const median = sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
  els.statRtt.replaceChildren(String(ms(v)), node('small', '', 'ms'));
  els.kvRtt.textContent = `last ${ms(v)} ms, median ${ms(median)} ms over ${plural(rtts.length, 'pulse')}`;
}

// ---------------- stats ----------------
function onStats(ev) {
  let snap = ev.data;
  if (typeof snap === 'string') {
    try { snap = JSON.parse(snap); } catch (_) { /* leave as string */ }
  }
  if (!snap || typeof snap !== 'object') {
    els.statsRaw.textContent = String(snap);
    return;
  }
  statsState = 'ok';
  lastStatsAt = Date.now();
  lastSnap = snap;
  if (typeof snap.connections === 'number') connections = snap.connections;
  if (typeof snap.published === 'number') {
    const t = performance.now();
    if (prevPublished && snap.published >= prevPublished.v && t > prevPublished.t) {
      const r = (snap.published - prevPublished.v) / ((t - prevPublished.t) / 1000);
      rateSamples.push({ rate: r, at: Date.now() });
      while (rateSamples.length > SPARK_POINTS) rateSamples.shift();
      els.serverRate.textContent = fmtRate(r);
      renderSpark();
    }
    prevPublished = { v: snap.published, t };
  }
  els.statsGrid.hidden = false;
  els.statsGrid.querySelectorAll('[data-stat]').forEach((n) => {
    const k = n.dataset.stat;
    if (k in snap) n.textContent = typeof snap[k] === 'number' ? snap[k].toLocaleString() : String(snap[k]);
  });
  els.statsRaw.textContent = JSON.stringify(snap, null, 2);
  updateStatsAge();
  renderStatNumbers();
  refreshPeers();
}

const fmtRate = (r) => (r === 0 ? '0' : r < 10 ? r.toFixed(1) : Math.round(r).toLocaleString());

// Tabs in this browser, on this channel, first heard from after the latest
// stats snapshot arrived: the snapshot cannot have counted them yet.
function bcSinceSnapshot() {
  let n = 0;
  for (const p of peers.values()) if (p.bc && p.bcSince > lastStatsAt) n += 1;
  return n;
}

// The server's count, plus the tabs in this browser that opened after the
// last 5 s snapshot, and never fewer than the live tabs this browser can see.
// The hood's stats tiles keep the raw snapshot.
function connectionsShown() {
  if (connections === null) return null;
  let local = 0;
  for (const p of peers.values()) if (p.bc) local += 1;
  return Math.max(connections + bcSinceSnapshot(), (mySid ? 1 : 0) + local);
}

// The Connections number and the plain-English sentence under the strip.
function renderStatNumbers() {
  const dd = els.statConns;
  const say = els.statsSay;
  // No live socket: a count would be a leftover from before the drop.
  if (client && (connState === 'reconnecting' || serverSilent())) {
    dd.textContent = '–';
    say.textContent = connState === 'reconnecting'
      ? 'The connection dropped, so the server count is on hold. It comes back once this tab reconnects.'
      : 'Server numbers appear once this tab is connected.';
    return;
  }
  const pending = !!client && statsPending();
  const shown = connectionsShown();
  if (shown !== null && client) {
    dd.textContent = shown.toLocaleString();
  } else if (pending) {
    dd.replaceChildren(node('span', 'skel'), node('span', 'visually-hidden', 'counting'));
    dd.firstChild.setAttribute('aria-hidden', 'true');
  } else {
    dd.textContent = statsState === 'refused' || statsState === 'failed' ? 'n/a' : '–';
  }
  if (!client) {
    say.textContent = connState === 'badkey' || !activeKey ? '' : 'Server numbers appear once this tab is connected.';
  } else if (lastSnap && shown !== null) {
    const s = lastSnap;
    const num = (k) => (typeof s[k] === 'number' ? s[k] : 0);
    say.replaceChildren(
      'The server is holding ', node('b', null, plural(shown, 'connection')),
      ` on ${plural(num('channels'), 'channel')}. Since it started it has fanned out `,
      node('b', null, plural(num('published'), 'message')),
      ` and dropped ${num('dropped').toLocaleString()}.`,
    );
  } else if (statsState === 'refused') {
    say.textContent = 'This server keeps its stats channel closed, so the connection count is unavailable.';
  } else if (statsState === 'failed') {
    say.textContent = 'Server numbers are not available right now; this tab will ask again shortly.';
  } else {
    say.replaceChildren(node('span', 'counting', 'Counting connections on the server'));
  }
}

function updateStatsAge() {
  if (!lastStatsAt) { els.statsAge.textContent = '–'; return; }
  const age = Math.floor((Date.now() - lastStatsAt) / 1000);
  const stale = Date.now() - lastStatsAt > STATS_STALE_MS;
  els.statsAge.textContent = stale ? `stale, last update ${age} s ago` : `updated ${age} s ago`;
  els.statsAge.classList.toggle('is-stale', stale);
}

function updateRate() {
  const now = performance.now();
  while (recvTimes.length && now - recvTimes[0] > RATE_WINDOW_MS) recvTimes.shift();
  els.statRate.textContent = fmtRate(recvTimes.length / (RATE_WINDOW_MS / 1000));
}

// Server-wide publishes per second, one point per stats snapshot.
const SVGNS = 'http://www.w3.org/2000/svg';
function svgEl(name, attrs) {
  const n = document.createElementNS(SVGNS, name);
  for (const k of Object.keys(attrs)) n.setAttribute(k, attrs[k]);
  return n;
}
function renderSpark() {
  const svg = els.sparkSvg;
  const w = Math.round(svg.clientWidth);
  if (!w) return; // hidden tab: drawn when the tab opens
  const h = 56;
  svg.setAttribute('viewBox', `0 0 ${w} ${h}`);
  const base = h - 1.5;
  const kids = [svgEl('line', { class: 'spark-base', x1: 0, x2: w, y1: base, y2: base })];
  els.sparkEmpty.hidden = rateSamples.length >= 2;
  if (rateSamples.length >= 2) {
    const peak = Math.max(...rateSamples.map((s) => s.rate));
    const max = Math.max(1, peak) * 1.15;
    const step = (w - 8) / (SPARK_POINTS - 1);
    const pts = rateSamples.map((s, i) => [
      w - 4 - (rateSamples.length - 1 - i) * step,
      base - (s.rate / max) * (h - 10),
    ]);
    const line = pts.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)} ${y.toFixed(1)}`).join(' ');
    const area = `${line} L${pts[pts.length - 1][0].toFixed(1)} ${base} L${pts[0][0].toFixed(1)} ${base} Z`;
    kids.push(svgEl('path', { class: 'spark-area', d: area }), svgEl('path', { class: 'spark-line', d: line }));
    const [lx, ly] = pts[pts.length - 1];
    kids.push(svgEl('circle', { class: 'spark-dot', cx: lx, cy: ly, r: 3 }));
    const now = rateSamples[rateSamples.length - 1].rate;
    svg.setAttribute('aria-label', `Server publishes per second over the last ${rateSamples.length * 5} seconds: now ${fmtRate(now)}, peak ${fmtRate(peak)}.`);
  }
  svg.replaceChildren(...kids);
}
new ResizeObserver(() => renderSpark()).observe(els.sparkSvg);

// Display refresh only; this timer never sends anything.
setInterval(() => {
  updateRate();
  updateStatsAge();
}, 500);

// ---------------- publishing ----------------
function canPublish() {
  return connState === 'live' && client && client.state === 'connected' && mySid;
}

let coolTimer = null;
function coolDown() {
  const now = performance.now();
  pruneSent(now);
  const waitMs = Math.max(120, Math.ceil(sentAt.length ? sentAt[0] + PULSE_WINDOW_MS - now : 0));
  els.btnPulse.classList.add('is-cooling');
  clearTimeout(coolTimer);
  coolTimer = setTimeout(() => els.btnPulse.classList.remove('is-cooling'), waitMs);
  showNotice('Pulses are capped at 4 a second per tab, because every visitor shares this demo key.', 'info', 2600);
}

function sendPulse() {
  if (!canPublish()) return;
  if (!takeToken()) { coolDown(); return; }
  pulseSeq += 1;
  const n = pulseSeq;
  const hubAt = diagram.beginOwnPulse();
  ownPending.set(n, { t0: performance.now(), hubAt, peers: peerView.peers });
  // Forget pulses whose echo never came (refused, or lost to a reconnect).
  for (const [k, p] of ownPending) if (performance.now() - p.t0 > 10_000) ownPending.delete(k);
  try {
    client.publish(CHANNEL, { kind: 'pulse', n, _from: mySid });
    code([`client.publish(${q(CHANNEL)}, { kind: "pulse", n: ${n}, _from: ${q(mySid)} });`], 'publish');
    if (peerView.peers === 0) { soloPulses += 1; renderHint(); }
  } catch (e) {
    ownPending.delete(n);
    diagram.refuse();
    showNotice(`Not sent: ${describe(e)}`, 'err');
  }
}

function publishCustom() {
  if (!canPublish()) return;
  if (!takeToken()) { coolDown(); return; }
  const raw = els.pubBody.value.trim();
  let data;
  try { data = JSON.parse(raw); } catch (_) { data = raw; }
  // Objects only: a string or number payload is never promoted to an object.
  if (data !== null && typeof data === 'object' && !Array.isArray(data)) data = { ...data, _from: mySid };
  try {
    client.publish(CHANNEL, data);
    let shown = JSON.stringify(data);
    if (shown.length > 140) shown = `${shown.slice(0, 139)}…`;
    code([`client.publish(${q(CHANNEL)}, ${shown});`], 'publish');
  } catch (e) {
    showNotice(`Not sent: ${describe(e)}`, 'err');
  }
}

// ---------------- sharing ----------------
function shareUrl() {
  const u = new URL(location.href);
  u.hash = '';
  return u.toString();
}
const useShare = () => canShare && coarsePointer.matches;
function renderShareLabel() {
  const label = useShare() ? 'Share link' : 'Copy link';
  els.copyLabel.textContent = label;
  els.btnCopy.setAttribute('aria-label', label);
  els.btnCopy.dataset.tip = label;
}
function legacyCopy(text) {
  const ta = node('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}
let copiedTimer = null;
async function copyLink() {
  const url = shareUrl();
  if (useShare()) {
    try {
      await navigator.share({ title: 'wirefan live demo', text: 'Send once. Every tab gets it.', url });
      return;
    } catch (e) {
      if (e && e.name === 'AbortError') return;
      // fall through to the clipboard
    }
  }
  let ok = false;
  try {
    await navigator.clipboard.writeText(url);
    ok = true;
  } catch (_) {
    ok = legacyCopy(url);
  }
  if (!ok) {
    showNotice(`Copy this link: ${url}`, 'info', 9000);
    return;
  }
  els.btnCopy.classList.add('is-done');
  els.copyLabel.textContent = 'Copied';
  els.btnCopy.dataset.tip = 'Copied';
  clearTimeout(copiedTimer);
  copiedTimer = setTimeout(() => {
    els.btnCopy.classList.remove('is-done');
    renderShareLabel();
  }, 1800);
  showNotice('Link copied. Open it on another device or in another browser, then send from either one.', 'info', 3600);
}

// ---------------- stress test (dev only) ----------------
// Raw WebSockets on purpose: phantom connections should NOT reconnect or
// resubscribe; they exist only to push the live connection counter up. On
// the shared public key that inflated every visitor's count, so the button
// is shown only with &dev=1.
async function runStress() {
  if (!DEV || stressActive || !activeKey) return;
  stressActive = true;
  els.btnStress.disabled = true;
  let opened = 0;
  let closed = 0;
  const label = () => { els.stressStatus.textContent = `opened ${opened}/${STRESS_CONNS}, closed ${closed}`; };
  label();
  const sockets = [];
  for (let i = 0; i < STRESS_CONNS; i++) {
    try {
      const s = new WebSocket(endpointFor(activeKey));
      s.onopen = () => { opened += 1; label(); };
      s.onclose = () => { closed += 1; label(); };
      s.onerror = () => { /* the close handler counts it */ };
      sockets.push(s);
    } catch (e) {
      showNotice(`Stress dial failed: ${e}`, 'err');
    }
    if (i % 10 === 9) await sleep(30);
  }
  setTimeout(() => {
    sockets.forEach((s) => { try { s.close(1000, 'stress-end'); } catch (_) { /* ignore */ } });
    stressActive = false;
    els.btnStress.disabled = connState !== 'live';
    els.stressStatus.textContent = `done: opened ${opened}, closed ${closed}`;
  }, STRESS_HOLD_MS);
}

// ---------------- developer panel ----------------
const tabs = [...document.querySelectorAll('.tab')];
function selectTab(tab, focus = false) {
  for (const t of tabs) {
    const on = t === tab;
    t.setAttribute('aria-selected', String(on));
    t.tabIndex = on ? 0 : -1;
    document.getElementById(t.getAttribute('aria-controls')).hidden = !on;
  }
  if (focus) tab.focus();
  try { localStorage.setItem(TAB_KEY, tab.id); } catch (_) { /* ignore */ }
  if (tab.id === 'tab-conn') renderSpark();
}
tabs.forEach((t, i) => {
  t.addEventListener('click', () => selectTab(t));
  t.addEventListener('keydown', (e) => {
    let j = null;
    if (e.key === 'ArrowRight') j = (i + 1) % tabs.length;
    else if (e.key === 'ArrowLeft') j = (i - 1 + tabs.length) % tabs.length;
    else if (e.key === 'Home') j = 0;
    else if (e.key === 'End') j = tabs.length - 1;
    if (j === null) return;
    e.preventDefault();
    selectTab(tabs[j], true);
  });
});

function openHood(tabId) {
  els.hood.open = true;
  const tab = document.getElementById(tabId);
  if (tab) selectTab(tab);
  els.hood.scrollIntoView({ behavior: reducedMotion.matches ? 'auto' : 'smooth', block: 'start' });
  setTimeout(() => (tab || els.hood.querySelector('summary')).focus({ preventScroll: true }), reducedMotion.matches ? 0 : 350);
}

function setUrlKey(key) {
  const u = new URL(location.href);
  u.searchParams.set('key', key);
  history.replaceState(null, '', u);
  els.btnTab.href = location.href;
}
function useKey(key) {
  const k = key.trim();
  if (!k) return false;
  setUrlKey(k);
  els.apiKey.value = k;
  connect(k);
  return true;
}

try {
  if (localStorage.getItem(HOOD_KEY) === '1' || location.hash === '#hood') els.hood.open = true;
  const saved = document.getElementById(localStorage.getItem(TAB_KEY) || '');
  if (saved && tabs.includes(saved)) selectTab(saved);
} catch (_) { /* storage blocked: stay collapsed */ }
els.hood.addEventListener('toggle', () => {
  try { localStorage.setItem(HOOD_KEY, els.hood.open ? '1' : '0'); } catch (_) { /* ignore */ }
  if (els.hood.open) renderSpark();
});

// ---------------- wiring ----------------
els.btnPulse.addEventListener('click', sendPulse);
els.btnCopy.addEventListener('click', copyLink);
els.keyForm.addEventListener('submit', (e) => {
  e.preventDefault();
  if (!useKey(els.apiKey.value)) els.apiKey.focus();
});
els.overlayForm.addEventListener('submit', (e) => {
  e.preventDefault();
  if (!useKey(els.overlayKey.value)) els.overlayKey.focus();
});
els.btnDisconnect.addEventListener('click', disconnect);
els.pubForm.addEventListener('submit', (e) => { e.preventDefault(); publishCustom(); });
els.showStats.addEventListener('change', () => els.frames.classList.toggle('show-stats', els.showStats.checked));
els.btnClearFrames.addEventListener('click', () => {
  els.frames.replaceChildren(node('li', 'frames-empty', 'Cleared.'));
  framesEmpty = true;
  framesShown = 0;
  els.framesCount.textContent = '';
});
els.btnCopyCode.addEventListener('click', async () => {
  const text = els.codeBody.textContent;
  let ok = false;
  try { await navigator.clipboard.writeText(text); ok = true; } catch (_) { ok = legacyCopy(text); }
  els.btnCopyCode.textContent = ok ? 'Copied' : 'Copy failed';
  setTimeout(() => { els.btnCopyCode.textContent = 'Copy'; }, 1600);
});
els.btnStress.addEventListener('click', runStress);
reducedMotion.addEventListener('change', () => diagram.layout(true));
coarsePointer.addEventListener('change', () => { renderShareLabel(); renderHint(); });

els.stressRow.hidden = !DEV;
els.stageChannel.textContent = CHANNEL;
els.stageChannel.title = CHANNEL;
els.legendChannel.textContent = `#${CHANNEL}`;
els.pubChannel.textContent = CHANNEL;
els.btnTab.href = location.href;
els.apiKey.value = activeKey;
diagram.setChannel(CHANNEL);
renderShareLabel();
if (!activeKey) {
  code(['// No API key in this link yet. Once this tab connects, every call it makes shows up here.'], 'note');
}

if (activeKey) connect(activeKey);
else setConnState('idle');
