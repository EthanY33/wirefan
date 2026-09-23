// Live fanout diagram for the wirefan demo page.
//
// Draws the fan from the README art (docs/img/hero.png): this tab's publish
// wire enters the hub from the left, and the hub fans out on curves to one
// terminal per other connection. Everything it shows comes from real data,
// and it only claims what the page can back up. wirefan has no presence
// feature (see CLAUDE.md, "Deferred"), so terminals come in three kinds:
//   - peer: a tab confirmed on this channel (it sent a pulse here, or it is
//     another tab in this browser that said so over BroadcastChannel). Lit,
//     labelled, and the only kind that delivery comets fly to.
//   - counted: a connection the server counts in _wirefan-stats that this
//     tab cannot place on this channel (another key, another room, a tab
//     that has not sent anything). Drawn dim and dashed, never animated.
//   - open: an empty slot, drawn only to show where the next tab will land.

const NS = 'http://www.w3.org/2000/svg';
const PATH_LEN = 1000;        // every wire is normalised to this pathLength
const COMET_TAIL = 260;       // comet lengths, in PATH_LEN units
const COMET_HEAD = 46;
export const INBOUND_MS = 420;
const OUTBOUND_MS = 780;
const MAX_SPARK_NODES = 160;  // skip comets (but still flash) past this

function make(name, attrs, parent) {
  const node = document.createElementNS(NS, name);
  if (attrs) for (const k of Object.keys(attrs)) node.setAttribute(k, attrs[k]);
  if (parent) parent.appendChild(node);
  return node;
}

// Order in which slots light up. Spread over the fan (the level middle slot
// is not first, so a single peer still sits on a visible curve), then by
// bisection. A terminal that is already lit keeps its place when the count
// grows.
const FIRST_SLOTS = [0.3, 0.7, 0.12, 0.88, 0.5, 0.21, 0.79, 0.4, 0.6, 0.03, 0.97];
function slotOrder(count) {
  const order = [];
  const seen = new Set();
  const push = (i) => {
    if (i >= 0 && i < count && !seen.has(i)) { seen.add(i); order.push(i); }
  };
  for (const f of FIRST_SLOTS) push(Math.round(f * (count - 1)));
  for (let div = 2; div <= 64 && order.length < count; div *= 2) {
    for (let k = 1; k < div; k += 2) push(Math.round((k / div) * (count - 1)));
  }
  for (let i = 0; i < count; i++) push(i);
  return order;
}

export class FanoutDiagram {
  /**
   * @param {SVGSVGElement} svg
   * @param {{ reducedMotion: MediaQueryList }} opts
   */
  constructor(svg, { reducedMotion }) {
    this.svg = svg;
    this.reduced = reducedMotion;
    this.desc = svg.querySelector('desc');
    this.meShort = '';
    this.channel = '';
    this.peers = [];           // [{ sid, label }] confirmed on this channel, most recent first
    this.counted = 0;          // other connections the server counts but this tab cannot place
    this.slotOf = new Map();   // sid -> slot index
    this.slots = [];
    this.lit = [];             // slot indexes holding a confirmed peer
    this.order = [];
    this.compact = false;
    this.W = 0;
    this.H = 0;
    this.build();
    this.ro = new ResizeObserver(() => this.layout());
    this.ro.observe(svg);
    if (document.fonts && document.fonts.ready) {
      document.fonts.ready.then(() => this.layout(true)).catch(() => {});
    }
  }

  get still() { return this.reduced.matches; }

  build() {
    const s = this.svg;
    const defs = make('defs', null, s);

    const glow = make('radialGradient', { id: 'wf-glow' }, defs);
    make('stop', { offset: '0', 'stop-color': '#f0561c', 'stop-opacity': '0.42' }, glow);
    make('stop', { offset: '0.38', 'stop-color': '#f0561c', 'stop-opacity': '0.13' }, glow);
    make('stop', { offset: '1', 'stop-color': '#f0561c', 'stop-opacity': '0' }, glow);

    this.leadGrad = make('linearGradient', { id: 'wf-lead', gradientUnits: 'userSpaceOnUse' }, defs);
    make('stop', { offset: '0', 'stop-color': '#f0561c', 'stop-opacity': '0' }, this.leadGrad);
    make('stop', { offset: '1', 'stop-color': '#f0561c', 'stop-opacity': '0.85' }, this.leadGrad);

    this.tailGrad = make('linearGradient', { id: 'wf-tail', gradientUnits: 'userSpaceOnUse' }, defs);
    make('stop', { offset: '0', 'stop-color': '#f0561c', 'stop-opacity': '0.6' }, this.tailGrad);
    make('stop', { offset: '1', 'stop-color': '#f0561c', 'stop-opacity': '0' }, this.tailGrad);

    const blur = make('filter', { id: 'wf-bloom', x: '-50%', y: '-50%', width: '200%', height: '200%' }, defs);
    make('feGaussianBlur', { stdDeviation: '3', result: 'b' }, blur);
    const merge = make('feMerge', null, blur);
    make('feMergeNode', { in: 'b' }, merge);
    make('feMergeNode', { in: 'SourceGraphic' }, merge);

    this.glow = make('circle', { class: 'hub-glow', fill: 'url(#wf-glow)' }, s);
    this.gWires = make('g', { class: 'wires' }, s);
    this.lead = make('line', { class: 'lead' }, s);
    this.mine = make('path', { class: 'wire-me', pathLength: PATH_LEN }, s);
    this.gTails = make('g', { class: 'tails' }, s);
    this.gSparks = make('g', { class: 'sparks', filter: 'url(#wf-bloom)' }, s);
    this.gTerms = make('g', { class: 'terms' }, s);

    const hub = make('g', { class: 'hub' }, s);
    this.hubHalo = make('circle', { class: 'hub-halo', r: '14' }, hub);
    this.hubRing = make('circle', { class: 'hub-ring', r: '14' }, hub);
    this.hubCore = make('circle', { class: 'hub-core', r: '6.5' }, hub);

    const me = make('g', { class: 'me' }, s);
    this.meHalo = make('rect', { class: 'me-halo', rx: '7' }, me);
    this.meTag = make('rect', { class: 'me-tag', rx: '5' }, me);
    this.meText = make('text', { class: 'me-text' }, me);

    this.gText = make('g', { class: 'annots' }, s);
    this.annPublish = this.annot('01', 'PUBLISH');
    this.annFan = this.annot('02', 'FAN OUT');
    this.annDeliver = this.annot('03', 'DELIVER');
  }

  annot(num, word) {
    const t = make('text', { class: 'annot' }, this.gText);
    const n = make('tspan', { class: 'annot-num' }, t);
    n.textContent = num;
    const w = make('tspan', { dx: '7' }, t);
    w.textContent = word;
    return t;
  }

  layout(force = false) {
    const rect = this.svg.getBoundingClientRect();
    const W = Math.max(280, Math.round(rect.width));
    const H = Math.max(220, Math.round(rect.height));
    if (!force && W === this.W && H === this.H) return;
    this.W = W;
    this.H = H;
    this.svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    const compact = W < 600;
    this.compact = compact;

    const padTop = compact ? 82 : 96;
    const padBot = compact ? 58 : 84;
    const cy = Math.round((padTop + (H - padBot)) / 2);

    // This tab: an ember tag sitting on the publish wire, as in the README art.
    this.meText.textContent = this.tagText();
    const tagX = compact ? 12 : 28;
    const tagH = 26;
    const textW = this.meText.getComputedTextLength() || 90;
    const tagW = Math.round(textW + 22);
    const tagRight = tagX + tagW;
    this.meTag.setAttribute('x', tagX);
    this.meTag.setAttribute('y', cy - tagH / 2);
    this.meTag.setAttribute('width', tagW);
    this.meTag.setAttribute('height', tagH);
    this.meHalo.setAttribute('x', tagX - 5);
    this.meHalo.setAttribute('y', cy - tagH / 2 - 5);
    this.meHalo.setAttribute('width', tagW + 10);
    this.meHalo.setAttribute('height', tagH + 10);
    this.meText.setAttribute('x', tagX + 11);
    this.meText.setAttribute('y', cy + 4);

    const hubX = Math.round(Math.max(tagRight + (compact ? 50 : 120), W * (compact ? 0.45 : 0.4)));
    const termX = W - (compact ? 58 : 92);
    for (const c of [this.hubHalo, this.hubRing, this.hubCore]) {
      c.setAttribute('cx', hubX);
      c.setAttribute('cy', cy);
    }
    this.glow.setAttribute('cx', hubX);
    this.glow.setAttribute('cy', cy);
    this.glow.setAttribute('r', Math.round(Math.min(W * 0.42, H * 0.62)));

    this.lead.setAttribute('x1', 0);
    this.lead.setAttribute('x2', tagX);
    this.lead.setAttribute('y1', cy);
    this.lead.setAttribute('y2', cy);
    this.leadGrad.setAttribute('x1', 0);
    this.leadGrad.setAttribute('x2', tagX);
    this.mineD = `M ${tagRight} ${cy} L ${hubX - 14} ${cy}`;
    this.mine.setAttribute('d', this.mineD);
    this.tailGrad.setAttribute('x1', termX);
    this.tailGrad.setAttribute('x2', W);

    // Terminal slots: centred on the hub, odd count so one sits level with
    // it, spread to fill the stage height within a comfortable spacing.
    const avail = H - padTop - padBot;
    const minGap = compact ? 20 : 27;
    let count = Math.floor(avail / minGap) + 1;
    count = Math.max(5, Math.min(17, count));
    if (count % 2 === 0) count -= 1;
    const spacing = Math.max(compact ? 18 : 22, Math.min(compact ? 26 : 34, avail / (count - 1)));
    this.order = slotOrder(count);

    this.gWires.replaceChildren();
    this.gTails.replaceChildren();
    this.gTerms.replaceChildren();
    this.slots = [];
    const dx = termX - hubX;
    for (let i = 0; i < count; i++) {
      const y = Math.round(cy + (i - (count - 1) / 2) * spacing);
      const d = `M ${hubX} ${cy} C ${hubX + dx * 0.5} ${cy}, ${termX - dx * 0.55} ${y}, ${termX} ${y}`;
      const wire = make('path', { class: 'wire', d, pathLength: PATH_LEN }, this.gWires);
      const tail = make('line', { class: 'tail', x1: termX + 6, x2: W, y1: y, y2: y }, this.gTails);
      const halo = make('circle', { class: 'term-halo', cx: termX, cy: y, r: '6' }, this.gTerms);
      const term = make('circle', { class: 'term', cx: termX, cy: y, r: '3.2' }, this.gTerms);
      const label = make('text', { class: 'peer-label', x: termX + 12, y: y - 6 }, this.gTerms);
      this.slots.push({ y, d, wire, tail, halo, term, label });
    }

    // Annotations: the three steps of a fanout, in the page's mono voice.
    this.annPublish.setAttribute('x', tagX);
    this.annPublish.setAttribute('y', cy - tagH / 2 - 16);
    this.annFan.setAttribute('x', hubX);
    this.annFan.setAttribute('y', cy + 44);
    this.annFan.setAttribute('text-anchor', 'middle');
    this.annDeliver.setAttribute('x', termX);
    this.annDeliver.setAttribute('y', this.slots[0].y - 22);
    this.annDeliver.setAttribute('text-anchor', 'middle');

    this.applyPeers();
  }

  tagText() {
    const base = this.compact ? 'YOU' : 'THIS TAB';
    return this.meShort ? `${base} ·${this.meShort}` : base;
  }

  /** Short id (last 4 chars of socket_id) for this tab, or '' when offline. */
  setMe(short) {
    this.meShort = short || '';
    this.layout(true);
  }

  /** Channel name, used only in the accessible description. */
  setChannel(name) {
    this.channel = name || '';
    this.applyPeers();
  }

  /**
   * @param {{sid: string, label: string}[]} peers  tabs confirmed on this channel, most recent first
   * @param {number} counted  other connections the server counts that this tab cannot place
   */
  setPeers(peers, counted) {
    this.peers = peers;
    this.counted = Math.max(0, counted | 0);
    this.applyPeers();
  }

  /** How many terminals are drawn in each state (capped by the slot count). */
  get drawn() {
    const count = this.slots.length;
    const peers = Math.min(this.peers.length, count);
    return { peers, counted: Math.min(this.counted, count - peers), slots: count };
  }

  setOffline(off) {
    this.svg.classList.toggle('is-offline', off);
  }

  applyPeers() {
    const count = this.slots.length;
    if (!count) return;
    const { peers: nPeers, counted: nCounted } = this.drawn;
    const occupied = this.order.slice(0, nPeers + nCounted);
    const occSet = new Set(occupied);

    // Keep a peer on the slot it already had while that slot stays
    // occupied, so a terminal never jumps when the counts change.
    const next = new Map();
    const taken = new Set();
    const shown = this.peers.slice(0, nPeers);
    for (const p of shown) {
      const prev = this.slotOf.get(p.sid);
      if (prev !== undefined && occSet.has(prev) && !taken.has(prev)) {
        next.set(p.sid, prev);
        taken.add(prev);
      }
    }
    for (const p of shown) {
      if (next.has(p.sid)) continue;
      const free = occupied.find((i) => !taken.has(i));
      if (free === undefined) break;
      next.set(p.sid, free);
      taken.add(free);
    }
    this.slotOf = next;
    const labelAt = new Map();
    for (const p of shown) if (next.has(p.sid)) labelAt.set(next.get(p.sid), p.label);

    this.lit = [...taken].sort((a, b) => a - b);
    this.slots.forEach((slot, i) => {
      const peer = taken.has(i);
      const counted = !peer && occSet.has(i);
      for (const el of [slot.wire, slot.tail, slot.term]) {
        el.classList.toggle('is-lit', peer);
        el.classList.toggle('is-counted', counted);
      }
      slot.term.setAttribute('r', peer ? '4.2' : counted ? '3.6' : '3.2');
      slot.label.textContent = labelAt.get(i) || '';
    });
    this.svg.classList.toggle('is-alone', nPeers + nCounted === 0);

    if (this.desc) {
      const ch = this.channel ? `#${this.channel}` : 'this channel';
      const p = this.peers.length;
      const c = this.counted;
      let text = 'This tab is wired to the wirefan hub.';
      text += p === 0
        ? ` No other tab is confirmed on ${ch} yet.`
        : ` The hub fans its pulses out to ${p} other tab${p === 1 ? '' : 's'} on ${ch}.`;
      if (c > 0) text += ` The server also counts ${c} other connection${c === 1 ? '' : 's'} this tab cannot place on ${ch}.`;
      this.desc.textContent = text;
    }
  }

  /** Slot for a sender: its peer slot, or null when this tab cannot place it. */
  slotFor(sid) {
    return sid && this.slotOf.has(sid) ? this.slotOf.get(sid) : null;
  }

  /**
   * Start this tab's own pulse: the publish wire into the hub. Returns the
   * performance.now() time the comet reaches the hub, so the fanout can be
   * held until then when the echo comes back faster than the animation.
   */
  beginOwnPulse() {
    const now = performance.now();
    if (document.hidden) return now;
    this.comet(this.mineD, false, 0, INBOUND_MS, 'in');
    this.flash(this.meHalo, 0, 'me');
    this.flashHub(INBOUND_MS);
    return now + INBOUND_MS;
  }

  /**
   * Animate one event reaching this tab. `from` is 'me', a socket_id, or
   * null for a publisher that did not say who it was. With `hubAt` the
   * inbound leg is already running (this tab's own pulse).
   */
  deliver({ from, hubAt = null }) {
    if (document.hidden || !this.slots.length) return;
    let delay = 0;
    if (hubAt !== null) {
      delay = Math.max(0, hubAt - performance.now());
    } else if (from === 'me') {
      this.comet(this.mineD, false, 0, INBOUND_MS, 'in');
      this.flash(this.meHalo, 0, 'me');
      this.flashHub(INBOUND_MS);
      delay = INBOUND_MS;
    } else {
      const src = this.slotFor(from);
      if (src !== null) {
        const slot = this.slots[src];
        this.comet(slot.d, true, 0, INBOUND_MS, 'in');
        this.flash(slot.halo, 0, 'term');
        delay = INBOUND_MS;
      }
      this.flashHub(delay);
    }
    for (const i of this.lit) {
      const slot = this.slots[i];
      this.comet(slot.d, false, delay, OUTBOUND_MS, 'out');
      this.flash(slot.halo, delay + OUTBOUND_MS * 0.7, 'term');
    }
    this.comet(this.mineD, true, delay, OUTBOUND_MS * 0.75, 'out');
    this.flash(this.meHalo, delay + OUTBOUND_MS * 0.55, 'me');
  }

  /** The server refused this tab's publish: the hub flashes and nothing fans out. */
  refuse() {
    this.svg.classList.add('is-refused');
    setTimeout(() => this.svg.classList.remove('is-refused'), 700);
  }

  // A comet: two dashes (soft tail, bright head) sliding along a wire.
  comet(d, reverse, delay, dur, leg) {
    if (this.still) {
      this.hotPath(d, delay, dur);
      return;
    }
    if (this.gSparks.childElementCount > MAX_SPARK_NODES) return;
    const easing = leg === 'in' ? 'cubic-bezier(.55,0,.85,.55)' : 'cubic-bezier(.2,.62,.35,1)';
    for (const [cls, dash] of [['spark-tail', COMET_TAIL], ['spark-head', COMET_HEAD]]) {
      const p = make('path', {
        class: cls, d, pathLength: PATH_LEN,
        'stroke-dasharray': `${dash} ${PATH_LEN * 3}`,
      }, this.gSparks);
      // Head position h runs 0 -> PATH_LEN + COMET_TAIL (forward) or
      // PATH_LEN -> -COMET_TAIL (reverse); both layers share it, so the
      // bright head always leads its soft tail.
      const from = reverse ? -PATH_LEN : dash;
      const to = reverse ? COMET_TAIL : dash - PATH_LEN - COMET_TAIL;
      p.style.strokeDashoffset = String(from);
      const anim = p.animate(
        [{ strokeDashoffset: from }, { strokeDashoffset: to }],
        { duration: dur, delay, easing, fill: 'both' },
      );
      anim.finished.then(() => p.remove(), () => p.remove());
    }
  }

  // Reduced motion: no travelling comets; the wire lights up and fades.
  hotPath(d, delay, dur) {
    const slot = this.slots.find((s) => s.d === d);
    const el = d === this.mineD ? this.mine : slot && slot.wire;
    if (el) this.hot(el, delay, dur);
  }

  hot(el, delay, dur) {
    setTimeout(() => {
      el.classList.add('is-hot');
      setTimeout(() => el.classList.remove('is-hot'), Math.max(260, dur * 0.8));
    }, delay);
  }

  flash(el, delay, kind) {
    if (this.still) {
      this.hot(el, delay, 420);
      return;
    }
    const frames = kind === 'me'
      ? [{ opacity: 0.9, transform: 'scale(1)' }, { opacity: 0, transform: 'scale(1.18)' }]
      : [{ opacity: 1, transform: 'scale(0.7)' }, { opacity: 0, transform: 'scale(2.8)' }];
    el.animate(frames, { duration: 700, delay, easing: 'cubic-bezier(.2,.7,.3,1)' });
  }

  flashHub(delay) {
    if (this.still) {
      this.hot(this.hubRing, delay, 480);
      return;
    }
    this.hubHalo.animate(
      [{ opacity: 0.95, transform: 'scale(1)' }, { opacity: 0, transform: 'scale(3.4)' }],
      { duration: 760, delay, easing: 'cubic-bezier(.2,.7,.3,1)' },
    );
    this.glow.animate(
      [{ opacity: 0.8 }, { opacity: 1, offset: 0.25 }, { opacity: 0.8 }],
      { duration: 900, delay, easing: 'ease-out' },
    );
    this.hubCore.animate(
      [{ transform: 'scale(1.7)' }, { transform: 'scale(1)' }],
      { duration: 420, delay, easing: 'cubic-bezier(.2,.7,.3,1)' },
    );
  }
}
