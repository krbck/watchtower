// watchtower UI — faz-1 functional prototype (D-013/D-014/D-015/D-016).
// Pages: Overview (cluster map from the topology mapper) and Connectivity
// (ConnectivityCheck chains). Graphs render as HTML cards over a cytoscape
// canvas; data updates are incremental and never move the user's viewport
// (D-016: continuous background refresh must not reset zoom/pan).

// D-039 changed the overview layout (hub-and-spoke): the key version was
// bumped so positions saved for the old grid reset once (D-026). Stored
// shapes: card id → {rx, ry} (offset from its box's layout origin), box id
// → {dx, dy} (how far the user moved the box).
const POS_KEY = 'dp-topology-positions-v2';
const OLD_POS_KEYS = ['dp-topology-positions'];
const FLOW_KEY = 'dp-topology-flow'; // '0' = flow animation off (D-039)

const state = {
  page: 'overview',
  checks: [],
  selected: null,
  clusterGraph: null, // latest cluster topology from the mapper
  nsFilter: '',       // overview namespace filter
  cy: null,
  fitDone: false,     // viewport fitted once per graph lifetime
  cardDivs: new Map(), // node id -> overlay card element
  selNode: null,       // inspected node id
  selNeighbors: new Set(),
  modalCy: null,
  modalCards: new Map(),
  modalFocus: null,
  positions: loadPositions(),
  connMode: 'workload', // Connectivity page: 'workload' (checks) | 'host' (trace, D-038)
  trace: null,          // last host trace response
  traceBranch: -1,      // overview: highlighted branch (-1 = all)
  traceHL: null,        // overview highlight { nodes: Set, edges: Set('from→to') }
  hostsFetchedAt: 0,
  selTruncated: 0,      // B-002: chain nodes left out of the highlight (cap)
  upstream: null,       // B-001: { key, data, at, loading } for the selected check
  layoutMs: 0,          // last overview layout compute time (D-039)
  layout: null,         // last WTGraph.hubLayout result (box origins for drags)
  cardHeights: new Map(), // node id -> { html, h } measured card heights (D-025)
  flowOn: loadFlowOn(),   // flow animation toggle (D-039)
  flowRaf: 0,
  flowLast: 0,
  flowStats: { frames: 0, totalMs: 0, maxMs: 0 },
  openItems: new Set(),   // keys of expanded warning/step lists (node id + reason, never text)
  warnOpen: false,        // Warnings panel shown (D-040)
  warnHTML: '',           // last rendered panel list (skip identical re-renders)
  search: { results: [], total: 0, active: -1, open: false }, // global search (D-040)
};

function loadPositions() {
  try {
    for (const k of OLD_POS_KEYS) localStorage.removeItem(k);
    return JSON.parse(localStorage.getItem(POS_KEY) || '{}');
  } catch {
    return {};
  }
}

function persistPositions() {
  try {
    localStorage.setItem(POS_KEY, JSON.stringify(state.positions));
  } catch {
    // localStorage unavailable (private mode) — drag works, persistence not.
  }
}

// boxShift is how far the user dragged a namespace box away from its
// computed spot.
function boxShift(boxId) {
  const s = state.positions[boxId];
  return s && Number.isFinite(s.dx) ? s : { dx: 0, dy: 0 };
}

// layoutOrigin returns the top-left corner of the node's box as laid out
// plus the user's box shift; card offsets are stored relative to it, so a
// re-layout (new resources shift the ring) keeps a dragged card with its
// box instead of stretching the box towards a stale absolute position.
function layoutOrigin(boxId) {
  const b = state.layout && state.layout.boxes[boxId || ''];
  if (!b) return null;
  const s = boxShift(boxId);
  return { x: b.x + s.dx, y: b.y + s.dy };
}

function saveCardPosition(id, boxId, pos) {
  const o = layoutOrigin(boxId);
  if (!o) return;
  state.positions[id] = { rx: pos.x - o.x, ry: pos.y - o.y };
  persistPositions();
}

function saveBoxShift(boxId, dx, dy) {
  const s = boxShift(boxId);
  state.positions[boxId] = { dx: s.dx + dx, dy: s.dy + dy };
  persistPositions();
}

function loadFlowOn() {
  try {
    return localStorage.getItem(FLOW_KEY) !== '0';
  } catch {
    return true;
  }
}

const $ = (sel) => document.querySelector(sel);

const KIND_META = {
  namespace: { title: 'Namespace', icon: '▦' },
  deployment: { title: 'Deployment', icon: '⚙' },
  statefulset: { title: 'StatefulSet', icon: '⚙' },
  daemonset: { title: 'DaemonSet', icon: '⚙' },
  service: { title: 'Service', icon: '⛁' },
  pod: { title: 'Pod', icon: '▣' },
  port: { title: 'Port', icon: '⚓' },
  ingress: { title: 'Ingress', icon: '⇄' },
  ingressclass: { title: 'IngressClass', icon: '⇄' },
  host: { title: 'Host', icon: '☁' },
  address: { title: 'External address', icon: '☁' },
  gateway: { title: 'Gateway', icon: '⇄' },
  virtualservice: { title: 'VirtualService', icon: '⇄' },
  client: { title: 'Client', icon: '☁' },
  app: { title: 'App', icon: '▶' },
};

// Per-kind card widths (heights are derived from content — D-017).
const NODE_WIDTH = {
  namespace: 190,
  port: 140,
  client: 150,
  app: 170,
  default: 240,
};

function nodeWidth(kind) { return NODE_WIDTH[kind] || NODE_WIDTH.default; }

// Workload kinds share the deployment card styling (D-035; no new tokens, D-005).
const WORKLOAD_KINDS = new Set(['deployment', 'statefulset', 'daemonset']);
// Istio routing kinds and ingress classes share the ingress card styling
// (D-037/D-038); trace-only host/address nodes reuse the client style.
const ROUTING_KINDS = new Set(['gateway', 'virtualservice', 'ingressclass']);
const TRACE_KINDS = new Set(['host', 'address']);
function styleKind(kind) {
  if (WORKLOAD_KINDS.has(kind)) return 'deployment';
  if (ROUTING_KINDS.has(kind)) return 'ingress';
  if (TRACE_KINDS.has(kind)) return 'client';
  return kind || 'unknown';
}

// CLUSTER_CELL is the synthetic header node of the "cluster" pseudo-cell
// that holds cluster-scoped nodes (IngressClass, later GatewayClass; D-038).
const CLUSTER_CELL = 'cell:cluster';

// cardHeight keeps the cytoscape hit box in sync with the HTML card so the
// last attribute row is never clipped.
function cardHeight(d) {
  const meta = d.meta || {};
  let rows = 1; // kind row
  if (d.label) rows++;
  rows += Object.keys(meta).length || (d.detail ? 1 : 0);
  if ((d.warnings || []).length) rows++;
  rows += portsRowCount(d);
  const portsExtra = (d.ports || []).length ? 12 + portsRowCount(d) * 3 : 0;
  return 42 + rows * 15 + portsExtra;
}

function portsRowCount(d) {
  const ports = d.ports || [];
  let rows = 0;
  for (const p of ports) {
    rows++;
    if (d.kind === 'service' && p.nodePort) rows++;
  }
  return rows;
}

function setConn(on) {
  $('#conn-dot').className = 'dot ' + (on ? 'on' : 'off');
  $('#conn-label').textContent = on ? 'live' : 'offline';
}

// --- navigation ---------------------------------------------------------

function switchPage(page) {
  state.page = page;
  for (const tab of document.querySelectorAll('#tabs .tab')) {
    tab.classList.toggle('active', tab.dataset.page === page);
  }
  $('#sidebar-overview').classList.toggle('hidden', page !== 'overview');
  $('#sidebar-connectivity').classList.toggle('hidden', page !== 'connectivity');
  // Page switch = new graph context: destroy and refit once.
  if (state.cy) { state.cy.destroy(); state.cy = null; }
  clearCards();
  state.selNode = null;
  state.selNeighbors = new Set();
  state.fitDone = false;
  render();
}

document.querySelectorAll('#tabs .tab').forEach((tab) =>
  tab.addEventListener('click', () => switchPage(tab.dataset.page))
);

function render() {
  if (state.page === 'overview') {
    const g = filteredGraph();
    $('#steps-title').textContent = 'map details';
    if (!g || g.nodes.length === 0) {
      setEmpty(state.clusterGraph ? 'no resources match the filter' : 'waiting for the topology mapper…');
      renderMapStats(null);
      setDetailsHint();
      return;
    }
    const els = toElements(g, true);
    syncGraph(els, g);
    renderMapStats(g);
    renderOverviewTrace();
    // Background sync must not wipe the inspector: re-render the selected
    // node from fresh data, or clear it only if the node is gone (D-020).
    if (state.selNode) {
      const live = state.cy ? state.cy.getElementById(state.selNode) : null;
      if (live && live.length) {
        inspectNode(live.data());
      } else {
        // The selected node vanished: its chain highlight goes with it and
        // an active host trace takes over again.
        state.selNode = null;
        state.selNeighbors = new Set();
        state.selTruncated = 0;
        if (state.cy) state.cy.elements().removeClass('focused dimmed');
        setDetailsHint();
        applyTraceHighlight();
        scheduleCardSync();
      }
    } else {
      setDetailsHint();
      applyTraceHighlight();
    }
  } else if (state.connMode === 'host') {
    $('#steps-title').textContent = 'trace';
    renderBranchList();
    const t = state.trace;
    if (!t || !t.topology || !(t.topology.nodes || []).length) {
      setEmpty(t ? 'no cluster path for this host — see the steps below' : 'enter a host to trace its path through the cluster');
      renderTraceSteps(t);
      return;
    }
    syncGraph(toElements({ nodes: withStepWarnings(t), edges: t.topology.edges || [] }), null);
    renderTraceSteps(t);
    applyTraceHighlight();
  } else {
    $('#steps-title').textContent = 'steps';
    const c = currentCheck();
    if (c) refreshUpstream(c);
    if (!c || !c.topology || !c.topology.nodes || c.topology.nodes.length === 0) {
      setEmpty(c ? 'waiting for the first reconcile…' : 'select a check or create one');
      renderWorkloadSteps(c);
      return;
    }
    const els = toElements(mergeUpstream(c));
    syncGraph(els, null);
    renderWorkloadSteps(c);
  }
}

// toElements converts API nodes/edges to cytoscape elements. Every optional
// field is always set (omitempty fields default explicitly) because
// syncGraph merges data into existing elements: a removed field must
// overwrite the stale value (qa D-037: warnings/ok stuck after a fix).
function toElements(g, overview) {
  const nsIDs = new Set(g.nodes.filter((n) => n.kind === 'namespace').map((n) => n.id));
  const hasCluster = overview && g.nodes.some((n) => n.kind !== 'namespace' && !n.namespace);
  const nodes = g.nodes.map((n) => {
    const data = Object.assign({}, n, {
      namespace: n.namespace || '',
      detail: n.detail || '',
      ok: n.ok ?? null,
      meta: n.meta || {},
      ports: n.ports || [],
      warnings: n.warnings || [],
      // Style selectors cannot test ok=false / kind sets directly.
      bad: n.ok === false || null,
      routing: ROUTING_KINDS.has(n.kind) || null,
      // Far-zoom tile colour, priority fail > warn > routing (D-040).
      tone: WTGraph.tileTone(n),
    });
    // Overview: namespaces are compound boxes; every resource sits inside
    // its namespace box instead of hanging off a namespace edge (D-039).
    if (overview && n.kind !== 'namespace') {
      const parent = n.namespace ? 'ns:' + n.namespace : (hasCluster ? CLUSTER_CELL : '');
      if (parent === CLUSTER_CELL || nsIDs.has(parent)) data.parent = parent;
    }
    // Only namespace boxes are draggable by cytoscape (the box moves its
    // children); cards are dragged through their HTML overlay (D-026).
    return { data, grabbable: !!overview && n.kind === 'namespace' };
  });
  // Cluster-scoped nodes (no namespace) live in a "cluster" pseudo-box; it
  // is a synthetic namespace-styled compound node.
  if (hasCluster) {
    nodes.push({ data: {
      id: CLUSTER_CELL, kind: 'namespace', label: 'cluster', namespace: '', cell: '',
      detail: 'cluster-scoped resources', ok: null, meta: {}, ports: [], warnings: [], tone: '',
    }, grabbable: true });
  }
  // Compound parents must exist before their children are added.
  nodes.sort((a, b) => (a.data.kind === 'namespace' ? 0 : 1) - (b.data.kind === 'namespace' ? 0 : 1));
  // Stable edge IDs (from→to + label) so incremental updates never keep a
  // stale endpoint under a reused positional id; duplicates get a suffix.
  const used = new Map();
  // Namespace membership is shown by the compound boxes on the Overview,
  // so the D-029/D-032 namespace edges are not drawn there (D-039).
  const allIDs = new Set(g.nodes.map((n) => n.id));
  const srcEdges = pruneEdges(overview ? g.edges.filter((e) => !nsIDs.has(e.from)) : g.edges, allIDs, 'toElements');
  const edges = srcEdges.map((e) => {
    let id = `${e.from}→${e.to}` + (e.label ? `|${e.label}` : '');
    const n = used.get(id) || 0;
    used.set(id, n + 1);
    if (n) id += `#${n}`;
    return { data: {
      id,
      source: e.from,
      target: e.to,
      label: e.label || '',
      ok: e.ok ?? null,
      // bad drives the red edge style (cytoscape cannot select ok=false
      // directly: "[ok = false]" is an invalid selector and matches all).
      bad: e.ok === false || null,
      ns: nsIDs.has(e.from) || null,
      config: e.config || null,
      // flow: traffic edges get the flowing-dash animation (D-039). On the
      // Overview only serves/binds/routes/selects; chain graphs (checks,
      // traces) are traffic paths edge by edge.
      flow: (!nsIDs.has(e.from) && !e.config && (!overview || WTGraph.TRAFFIC_RE.test(e.label || ''))) || null,
    } };
  });
  return { nodes, edges };
}

// pruneEdges drops edges with a missing endpoint (cy.add would throw and
// abort the render) and warns once per call site.
const warnedPrune = new Set();
function pruneEdges(edges, ids, where) {
  const kept = WTGraph.keepResolved(edges, ids);
  if (kept.length !== edges.length && !warnedPrune.has(where)) {
    warnedPrune.add(where);
    console.warn(`${where}: dropped ${edges.length - kept.length} edge(s) with a missing endpoint`);
  }
  return kept;
}

function setEmpty(text) {
  const empty = $('#graph-empty');
  empty.style.display = 'flex';
  empty.textContent = text;
  if (state.cy) { state.cy.destroy(); state.cy = null; }
  clearCards();
  state.fitDone = false;
}

function setDetailsHint() {
  $('#step-list').innerHTML = '<li class="muted">click a card in the map to inspect it</li>';
}

// --- graph sync (incremental, viewport-stable) ---------------------------

// syncGraph diffs the desired elements into the live cytoscape instance.
// It only re-runs the layout when the structure actually changed and never
// fits the viewport afterwards (D-016).
function syncGraph(els) {
  const empty = $('#graph-empty');
  empty.style.display = 'none';

  if (!state.cy) {
    state.cy = cytoscape({
      container: $('#cy'),
      elements: [],
      layout: { name: 'preset' },
      wheelSensitivity: 0.2,
      // Overview namespace boxes can be dragged (cards are dragged through
      // their HTML overlay, D-026); chain graphs stay fixed.
      autoungrabify: state.page !== 'overview',
      style: graphStyle(),
    });
    // Not 'render': the flow animation redraws the canvas every frame and
    // must not re-position 160 HTML cards each time (D-039).
    state.cy.on('pan zoom resize viewport', scheduleCardSync);
    state.cy.on('position', 'node', scheduleCardSync);
    state.cy.on('tap', (ev) => {
      // A namespace box behaves like the background.
      if (ev.target !== state.cy && !isBox(ev.target)) return;
      state.selNode = null;
      state.selNeighbors = new Set();
      state.selTruncated = 0;
      state.cy.elements().removeClass('focused dimmed');
      if (state.page === 'overview') setDetailsHint();
      else if (state.connMode === 'host') renderTraceSteps(state.trace);
      else renderWorkloadSteps(currentCheck());
      applyTraceHighlight();
      scheduleCardSync();
    });
    if (state.page === 'overview') attachBoxDrag(state.cy);
    startFlow();
  }

  const want = new Map();
  for (const n of els.nodes) want.set(n.data.id, n);
  for (const e of els.edges) want.set(e.data.id, e);

  let nodesChanged = false;
  let edgesChanged = false;

  // Remove elements that no longer exist.
  const stale = [];
  state.cy.elements().forEach((ele) => { if (!want.has(ele.id())) stale.push(ele); });
  if (stale.length) {
    for (const e of stale) { if (e.isNode()) nodesChanged = true; else edgesChanged = true; }
    state.cy.batch(() => stale.forEach((e) => e.remove()));
  }

  // Add/update the rest.
  state.cy.batch(() => {
    for (const el of els.nodes.concat(els.edges)) {
      const existing = state.cy.getElementById(el.data.id);
      if (existing.length) {
        existing.data(el.data);
      } else {
        state.cy.add(el);
        if (el.data.source) edgesChanged = true; else nodesChanged = true;
      }
    }
  });
  if (nodesChanged || edgesChanged) {
    flowEdges.delete(state.cy);
    state.cy.removeScratch('_wtFar'); // far-tile class for new elements
  }

  // The Overview only re-layouts when the node set changes: edge updates
  // from background syncs must not move anything (D-016/D-039). Chain
  // graphs are small dagre layouts driven by their edges.
  const needLayout = state.page === 'overview' ? nodesChanged : (nodesChanged || edgesChanged);
  if (needLayout && state.cy.nodes().length) {
    if (!state.fitDone) {
      runLayout();
      state.cy.fit(undefined, 60);
      if (state.page !== 'overview') keepChainReadable();
      state.fitDone = true;
    } else {
      // Preserve the user's viewport exactly across background refreshes.
      const zoom = state.cy.zoom();
      const pan = state.cy.pan();
      runLayout();
      state.cy.viewport({ zoom, pan });
    }
  }
  scheduleCardSync();
}

// keepChainReadable: a long chain fitted to the screen (workload check plus
// upstream chain, host trace) would land on unreadable mini cards. Keep a
// floor zoom and anchor the top of the chain; the rest is one pan away.
const CHAIN_MIN_ZOOM = 0.55;
function keepChainReadable() {
  const cy = state.cy;
  if (cy.zoom() >= CHAIN_MIN_ZOOM) return;
  cy.zoom(CHAIN_MIN_ZOOM);
  const bb = cy.elements().boundingBox();
  cy.pan({
    x: cy.width() / 2 - ((bb.x1 + bb.x2) / 2) * CHAIN_MIN_ZOOM,
    y: 40 - bb.y1 * CHAIN_MIN_ZOOM,
  });
}

function runLayout() {
  // Overview: hub-and-spoke preset layout around the ingress points (D-039).
  if (state.page === 'overview') {
    layoutOverview();
    return;
  }
  // Host traces are long single chains: tighter ranks keep them readable.
  const hostTrace = state.page === 'connectivity' && state.connMode === 'host';
  const c = currentCheck();
  const upstreamShown = state.page === 'connectivity' && state.connMode === 'workload' && c &&
    state.upstream && state.upstream.key === upstreamKey(c) && (((state.upstream.data || {}).hosts) || []).length > 0;
  state.cy.layout({
    name: 'dagre',
    rankDir: 'TB',
    nodeSep: 70,
    rankSep: hostTrace || upstreamShown ? 60 : 150,
    animate: false,
    fit: false,
  }).run();
}

// layoutOverview computes the hub-and-spoke layout (WTGraph.hubLayout) from
// measured card sizes and applies it as preset positions. User-dragged
// cards keep their offset inside their box; dragged boxes keep their shift
// (D-026).
function layoutOverview() {
  const cy = state.cy;
  if (!cy) return;
  const t0 = performance.now();
  const leaves = [];
  const boxes = [];
  cy.nodes().forEach((n) => {
    const d = n.data();
    if (d.kind === 'namespace') boxes.push({ id: d.id, label: d.label });
    else leaves.push(d);
  });
  measureCards(leaves);
  const nodes = leaves.map((d) => ({
    id: d.id,
    kind: d.kind,
    box: d.parent || '',
    label: d.label,
    w: nodeWidth(d.kind),
    h: layoutCardHeight(d),
    ghost: d.kind === 'ingressclass' && (d.meta || {}).status === 'not found',
  }));
  const edges = cy.edges().map((e) => ({
    source: e.data('source'),
    target: e.data('target'),
    config: !!e.data('config'),
  }));
  const res = WTGraph.hubLayout({ nodes, boxes, edges });
  state.layout = res;
  cy.batch(() => {
    cy.nodes().forEach((n) => {
      if (n.isParent()) return;
      const d = n.data();
      const boxId = d.kind === 'namespace' ? d.id : (d.parent || '');
      const shift = boxShift(boxId);
      const saved = state.positions[d.id];
      const origin = layoutOrigin(boxId);
      if (d.kind !== 'namespace' && saved && Number.isFinite(saved.rx) && origin) {
        n.position({ x: origin.x + saved.rx, y: origin.y + saved.ry });
      } else if (res.positions[d.id]) {
        n.position({ x: res.positions[d.id].x + shift.dx, y: res.positions[d.id].y + shift.dy });
      }
      if (d.kind !== 'namespace') n.style('height', layoutCardHeight(d));
    });
  });
  state.layoutMs = performance.now() - t0;
}

// measureCards renders new/changed cards off screen once to get their real
// height before the layout runs (D-025); all reads happen after all writes
// (one reflow).
function measureCards(list) {
  const todo = [];
  for (const d of list) {
    const html = cardHTML(d, false);
    const known = state.cardHeights.get(d.id);
    if (!known || known.html !== html) todo.push({ d, html });
  }
  if (!todo.length) return;
  const probe = document.createElement('div');
  probe.style.cssText = 'position:absolute;left:0;top:0;visibility:hidden';
  for (const t of todo) {
    t.div = document.createElement('div');
    t.div.className = 'node-card k-' + styleKind(t.d.kind);
    t.div.style.width = nodeWidth(t.d.kind) + 'px';
    t.div.innerHTML = t.html;
    probe.appendChild(t.div);
  }
  $('#labels').appendChild(probe);
  for (const t of todo) {
    state.cardHeights.set(t.d.id, { html: t.html, h: t.div.offsetHeight });
  }
  probe.remove();
}

function layoutCardHeight(d) {
  const m = state.cardHeights.get(d.id);
  return Math.max(m ? m.h : 0, cardHeight(d));
}

function isBox(ele) {
  return !!ele && ele.isNode && ele.isNode() && ele.data('kind') === 'namespace';
}

// attachBoxDrag persists namespace box drags; cytoscape moves the compound
// and its children by itself (D-039/D-026).
function attachBoxDrag(cy) {
  cy.on('grab', 'node[kind = "namespace"]', (ev) => {
    ev.target.scratch('_wtGrab', Object.assign({}, ev.target.position()));
  });
  cy.on('dragfree', 'node[kind = "namespace"]', (ev) => {
    const from = ev.target.scratch('_wtGrab');
    if (!from) return;
    const p = ev.target.position();
    saveBoxShift(ev.target.id(), p.x - from.x, p.y - from.y);
    ev.target.removeScratch('_wtGrab');
  });
}

// graphStyle is the shared cytoscape style: invisible node hit boxes (the
// visuals are the HTML cards) plus dim/focus rules for edge highlighting.
function graphStyle() {
  // Failed edges and namespace boxes use the existing tokens (D-005: the
  // stylesheet stays the single source; no new color values).
  const css = getComputedStyle(document.documentElement);
  const token = (name) => css.getPropertyValue(name).trim();
  const fail = token('--fail');
  const rules = [
    {
      selector: 'node',
      style: {
        'background-opacity': 0,
        'border-width': 0,
        'label': '',
        'width': (ele) => nodeWidth(ele.data('kind')),
        'height': (ele) => cardHeight(ele.data()),
        'shape': 'round-rectangle',
      },
    },
    {
      // Namespace compound box (D-039); its name is an HTML overlay label
      // (syncNsLabel).
      selector: 'node[kind = "namespace"]',
      style: {
        'background-color': token('--panel'),
        'background-opacity': 0.7,
        'border-width': 1.5,
        'border-color': token('--muted'),
        'border-opacity': 0.45,
        'padding': WTGraph.DEFAULTS.pad,
      },
    },
    {
      selector: 'node[kind = "namespace"]:childless',
      style: {
        'padding': 0,
        'width': WTGraph.DEFAULTS.emptyW,
        'height': WTGraph.DEFAULTS.emptyH,
      },
    },
    // Far zoom (D-019): cards are hidden; the hit boxes become plain tiles so
    // the hub-and-spoke structure stays visible behind the namespace pills.
    // Tile colour follows WTGraph.tileTone: fail > warn > routing (D-040).
    { selector: 'node.far-tile', style: { 'background-color': token('--muted'), 'background-opacity': 0.55 } },
    { selector: 'node.far-tile[tone = "routing"]', style: { 'background-color': token('--accent'), 'background-opacity': 0.85 } },
    { selector: 'node.far-tile[tone = "warn"]', style: { 'background-color': token('--warn'), 'background-opacity': 0.9 } },
    { selector: 'node.far-tile[tone = "fail"]', style: { 'background-color': fail, 'background-opacity': 0.9 } },
    { selector: 'node.dimmed', style: { 'opacity': 0 } },
    { selector: 'node[kind = "namespace"].dimmed', style: { 'opacity': 0.3 } },
    {
      selector: 'edge',
      style: {
        'width': 1.5,
        'curve-style': 'bezier',
        'target-arrow-shape': 'triangle',
        'arrow-scale': 0.9,
        'line-color': '#3d4b63',
        'target-arrow-color': '#3d4b63',
        'label': 'data(label)',
        'font-size': 7,
        'color': '#94a3b8',
        'text-background-color': '#0b0e14',
        'text-background-opacity': 1,
        'text-background-padding': 2,
      },
    },
    {
      // namespace containment lines and config relations (IngressClass →
      // Ingress, D-038): faint, dashed, no arrowhead (D-029 style)
      selector: 'edge[?ns], edge[?config]',
      style: {
        'width': 1,
        'line-style': 'dashed',
        'opacity': 0.35,
        'target-arrow-shape': 'none',
        'label': '',
      },
    },
    // Highlight rules come after the base edge rule so they win (later
    // cytoscape rules override earlier ones).
    { selector: 'edge.dimmed', style: { 'opacity': 0.08 } },
    {
      selector: 'edge.focused',
      style: {
        'width': 2.5,
        'line-color': '#22d3ee',
        'target-arrow-color': '#22d3ee',
      },
    },
  ];
  if (flowEnabled()) {
    // Traffic edges: dash pattern animated by flowTick (D-039). Comes after
    // the base and focus rules so highlights keep their width and color.
    rules.push({
      selector: 'edge[?flow]',
      style: { 'line-style': 'dashed', 'line-dash-pattern': FLOW_DASH, 'line-dash-offset': 0 },
    });
  }
  // Far zoom: thin solid edges read better than sub-pixel dashes.
  rules.push({
    selector: 'edge.far-tile[!config]',
    style: { 'width': 7, 'line-style': 'solid', 'label': '', 'line-color': token('--muted'), 'target-arrow-color': token('--muted') },
  });
  if (fail) {
    rules.push({ selector: 'edge[?bad]', style: { 'line-color': fail, 'target-arrow-color': fail } });
  }
  return rules;
}

// --- HTML card overlay (D-016) -------------------------------------------

let cardSyncScheduled = false;

function scheduleCardSync() {
  if (cardSyncScheduled) return;
  cardSyncScheduled = true;
  requestAnimationFrame(() => {
    cardSyncScheduled = false;
    syncCards();
  });
}

function clearCards() {
  $('#labels').innerHTML = '';
  state.cardDivs.clear();
}

// attachCardDrag lets users drag cards with the mouse; the dropped position
// is persisted in localStorage and survives page reloads (D-026).
function attachCardDrag(div, id) {
  let drag = null;
  div.addEventListener('pointerdown', (ev) => {
    if (ev.button !== 0 || ev.target.closest('.card-expand')) return;
    const cy = state.cy;
    if (!cy) return;
    const node = cy.getElementById(id);
    // Boxes are dragged on the canvas; far-zoom pills only inspect.
    if (!node.length || isBox(node)) return;
    drag = {
      node,
      x0: ev.clientX,
      y0: ev.clientY,
      px: node.position().x,
      py: node.position().y,
      moved: false,
    };
    div.style.cursor = 'grabbing';
    try { div.setPointerCapture(ev.pointerId); } catch {}
    ev.preventDefault();
  });
  div.addEventListener('pointermove', (ev) => {
    if (!drag) return;
    const zoom = state.cy.zoom();
    const dx = (ev.clientX - drag.x0) / zoom;
    const dy = (ev.clientY - drag.y0) / zoom;
    if (!drag.moved && Math.abs(dx) + Math.abs(dy) < 3) return;
    drag.moved = true;
    drag.node.position({ x: drag.px + dx, y: drag.py + dy });
  });
  div.addEventListener('pointerup', (ev) => {
    div.style.cursor = '';
    if (!drag) return;
    if (drag.moved) {
      if (state.page === 'overview') saveCardPosition(id, drag.node.data('parent'), drag.node.position());
      div._dragged = true;
      try { div.releasePointerCapture(ev.pointerId); } catch {}
    }
    drag = null;
  });
  div.addEventListener('pointercancel', () => { drag = null; div.style.cursor = ''; });
}

// Namespace label geometry: a continuous function of the zoom, so nothing
// jumps when the cards appear. Far out the name sits in the middle of the
// box; from NS_NEAR_ZOOM on, on its top edge.
const NS_FAR_ZOOM = 0.3;
const NS_NEAR_ZOOM = 0.42;
const NS_FAR_FONT = 12;
const NS_MIN_FONT = 9;
const clamp01 = (v) => Math.min(1, Math.max(0, v));

const nsTextCtx = document.createElement('canvas').getContext('2d');
const nsTextCache = new Map();
// nsTextWidth: rendered width of the label per px of font size.
function nsTextWidth(label) {
  let w = nsTextCache.get(label);
  if (w === undefined) {
    nsTextCtx.font = '700 100px ui-monospace, "JetBrains Mono", "Fira Code", monospace';
    w = nsTextCtx.measureText(label).width / 100;
    nsTextCache.set(label, w);
  }
  return w;
}

// syncNsLabel places the namespace name: shrunk (then ellipsized) to stay
// inside its box, roughly constant on screen when zoomed in.
function syncNsLabel(div, node, d, zoom, isSel, isDim) {
  const label = d.label || '';
  if (div._text !== label) {
    div.textContent = label;
    div.title = label;
    div._text = label;
  }
  div.className = 'ns-name' + (isSel ? ' sel' : '') + (isDim ? ' dim' : '');
  const raw = clamp01((zoom - NS_FAR_ZOOM) / (NS_NEAR_ZOOM - NS_FAR_ZOOM));
  const t = raw * raw * (3 - 2 * raw);
  const rb = node.renderedBoundingBox({ includeLabels: false, includeOverlays: false, includeUnderlays: false });
  const near = 14 + 2 * clamp01((zoom - 0.5) / 0.5);
  const wanted = NS_FAR_FONT + (near - NS_FAR_FONT) * t;
  const avail = Math.max(16, rb.w - 12);
  const font = Math.max(NS_MIN_FONT, Math.min(wanted, avail / nsTextWidth(label)));
  div.style.fontSize = font + 'px';
  div.style.maxWidth = avail + 'px';
  const midY = (rb.y1 + rb.y2) / 2;
  const topY = rb.y1 - 4 - (font * 1.3) / 2;
  div.style.left = (rb.x1 + rb.x2) / 2 + 'px';
  div.style.top = midY + (topY - midY) * t + 'px';
}

function syncCards() {
  if (!state.cy) { return; }
  const layer = $('#labels');

  // Drop cards whose nodes are gone.
  for (const [id, div] of state.cardDivs) {
    if (state.cy.getElementById(id).length === 0) {
      div.remove();
      state.cardDivs.delete(id);
    }
  }

  const zoom = state.cy.zoom();
  // Namespaces-only far view is an Overview feature; chain graphs
  // (checks, host traces) always show their cards.
  const far = state.page === 'overview' && zoom < 0.35;
  const compact = !far && zoom < 0.5;
  if (far !== state.cy.scratch('_wtFar')) {
    state.cy.scratch('_wtFar', far);
    state.cy.batch(() => {
      state.cy.elements().not('node[kind = "namespace"]').toggleClass('far-tile', far);
    });
  }

  state.cy.nodes().forEach((node) => {
    const d = node.data();
    const id = d.id;
    let div = state.cardDivs.get(id);
    if (!div) {
      div = document.createElement('div');
      div.addEventListener('click', (ev) => {
        const btn = ev.target.closest ? ev.target.closest('.card-expand') : null;
        if (btn) {
          ev.stopPropagation();
          openConnections(id);
          return;
        }
        if (div._dragged) { div._dragged = false; return; }
        const live = state.cy ? state.cy.getElementById(id) : null;
        inspectNode(live && live.length ? live.data() : d);
      });
      attachCardDrag(div, id);
      layer.appendChild(div);
      state.cardDivs.set(id, div);
    }
    const isSel = state.selNode === id;
    const hlActive = !!state.selNode ||
      (state.page === 'overview' && !!state.traceHL) ||
      (state.page === 'connectivity' && state.connMode === 'host' && state.traceBranch >= 0);
    const isDim = hlActive && !state.selNeighbors.has(id);
    const isNs = d.kind === 'namespace';

    // A namespace is its compound box on the canvas plus one plain-text
    // label, at every zoom level (D-039).
    if (isNs && state.page === 'overview') {
      div.style.display = '';
      syncNsLabel(div, node, d, zoom, isSel, isDim);
      return;
    }
    // Very far out: cards are hidden; the labels and tiles orient the user
    // (D-019).
    if (far) {
      div.style.display = 'none';
      return;
    }
    div.style.display = '';

    div.className = 'node-card k-' + styleKind(d.kind) +
      (d.ok === false ? ' bad' : '') + ((d.warnings || []).length ? ' warned' : '') +
      (isSel ? ' sel' : '') + (isDim ? ' dim' : '') + (compact ? ' compact' : '');
    const html = cardHTML(d, isSel);
    if (div._html !== html) { div.innerHTML = html; div._html = html; }
    div.style.width = nodeWidth(d.kind) + 'px';
    div.style.height = 'auto';
    // Scale the card with the graph so cards never overlap when zoomed out.
    div.style.transform = `translate(-50%, -50%) scale(${zoom})`;
    const rp = node.renderedPosition();
    div.style.left = rp.x + 'px';
    div.style.top = rp.y + 'px';
    // Compact: header only, sized to its content; the hit box keeps the
    // full card height so zooming out never reshuffles the layout.
    if (compact) return;
    // Measure the real content height (transform-independent) so the last
    // row/port line is never clipped, and keep the hit box in sync.
    const measured = Math.max(div.offsetHeight, cardHeight(d));
    div.style.height = measured + 'px';
    if (Math.abs(node.height() - measured) > 0.5) {
      node.style('height', measured);
    }
  });
}

// cardHTML renders the reference-style card: kind header + field rows.
function cardHTML(d, isSel) {
  const meta = d.meta || {};
  const km = KIND_META[d.kind] || { title: d.kind || '?', icon: '◻' };
  const rows = [];
  rows.push(field('kind', km.title));
  if (d.label) rows.push(field('name', shorten(d.label, 26)));
  for (const [k, v] of Object.entries(meta)) {
    const cls = k === 'containerPort' ? ' port-green' : (k === 'port' || k === 'ports' ? ' port-amber' : '');
    rows.push(field(k, shorten(String(v), 28), cls));
  }
  if (Object.keys(meta).length === 0 && d.detail) {
    rows.push(`<div class="row dim">${esc(d.detail)}</div>`);
  }
  const warns = d.warnings || [];
  if (warns.length) {
    rows.push(`<div class="row port-amber" title="${esc(warningsText(warns))}"><span class="fk">warnings</span>: ` +
      `<span class="fv">${warns.length} ⚠</span></div>`);
  }
  const expand = isSel ? '<button class="card-expand" title="show connections">⤢</button>' : '';
  const compactName = d.label ? `<span class="compact-name">${esc(d.label)}</span>` : '';
  // Compact (mid zoom) cards hide the body: the header carries the mark.
  const hdrWarn = warns.length ? `<span class="hdr-warn" title="${esc(warns.length)} warning(s)">⚠ ${esc(warns.length)}</span>` : '';
  return `<div class="card-header"><span class="icon">${km.icon}</span><span class="card-kind">${esc(km.title)}</span>${compactName}${hdrWarn}${expand}</div>` +
    `<div class="card-body">${rows.join('')}${portsBlock(d)}</div>`;
}

// portsBlock renders the structured port list: "port 80 → targetPort 3000"
// with a match mark, and nodePort on its own row (D-023).
function portsBlock(d) {
  const ports = d.ports || [];
  if (!ports.length) return '';
  const out = [];
  for (const p of ports) {
    if (d.kind === 'service') {
      const mark = p.matched
        ? '<span class="p-mark ok">✓</span>'
        : `<span class="p-mark bad" title="${esc(p.hint || p.reason || 'targetPort not matched')}">⚠</span>`;
      out.push(
        `<div class="port-row"><span class="p-k">port</span> <span class="p-v">${esc(p.port)}</span> ` +
        `<span class="p-arrow">→</span> <span class="p-k">targetPort</span> ` +
        `<span class="p-v">${esc(String(p.targetPort || p.port))}</span> ${mark}</div>`
      );
      if (p.nodePort) {
        out.push(`<div class="port-row"><span class="p-k">nodePort</span> <span class="p-v">${esc(p.nodePort)}</span></div>`);
      }
    } else {
      const proto = p.protocol && p.protocol !== 'TCP'
        ? ` <span class="p-np">${esc(p.protocol)}</span>`
        : '';
      out.push(`<div class="port-row"><span class="p-k">containerPort</span> <span class="p-v">${esc(p.port)}</span>${proto}</div>`);
    }
  }
  return `<div class="ports-block">${out.join('')}</div>`;
}

// warningsText flattens node warnings for a native tooltip (D-037).
function warningsText(warns) {
  return warns.map((w) => `⚠ ${w.message || w.reason}` + (w.hint ? `\n→ ${w.hint}` : '')).join('\n\n');
}

// warningsHTML renders node warnings with the existing step text styles.
// List open state is keyed by node id + reason (+ #n for a repeated
// reason), never by message text: two nodes with the same warning text no
// longer open together (qa D-039).
function warningsHTML(warns, nodeId) {
  const seen = new Map();
  return (warns || []).map((w) => {
    const base = `${nodeId || ''}|${w.reason || 'warning'}`;
    const n = (seen.get(base) || 0) + 1;
    seen.set(base, n);
    return `<div class="sname">⚠ ${esc(w.message || w.reason)}</div>` +
      itemsHTML(w.items, n > 1 ? `${base}#${n}` : base) +
      (w.hint ? `<div class="shint">→ ${esc(w.hint)}</div>` : '');
  }).join('');
}

// itemsHTML renders a long list moved out of a sentence (warning/step
// items) as an inline expand. Open state is kept in state.openItems across
// the 5s background re-render of the inspector.
function itemsHTML(items, key, noun) {
  const list = items || [];
  if (!list.length) return '';
  const what = noun || 'host';
  const label = `${list.length} ${list.length === 1 ? what : what + 's'}`;
  return `<details class="items" data-key="${esc(key)}"${state.openItems.has(key) ? ' open' : ''}>` +
    `<summary><span class="when-closed">show ${label}</span><span class="when-open">hide ${label}</span></summary>` +
    `<ul class="items-list">${list.map((v) => `<li>${esc(v)}</li>`).join('')}</ul></details>`;
}

// details elements report toggles on themselves only: capture on document.
document.addEventListener('toggle', (ev) => {
  const d = ev.target;
  if (!d.classList || !d.classList.contains('items')) return;
  if (d.open) state.openItems.add(d.dataset.key); else state.openItems.delete(d.dataset.key);
}, true);

function field(k, v, cls = '') {
  return `<div class="row${cls}"><span class="fk">${esc(k)}</span>: <span class="fv">${esc(v)}</span></div>`;
}

function inspectNode(d) {
  state.selNode = d.id;
  highlightNeighborhood(d.id);
  const chips = Object.entries(d.meta || {})
    .map(([k, v]) => `<span class="chip">${esc(k)}: ${esc(v)}</span>`)
    .join('');
  $('#step-list').innerHTML = `
    <li class="ok"><span class="mark">◆</span><div>
      <div class="sname">${esc(d.label || d.kind)}</div>
      <div class="sdetail">kind: ${esc(d.kind)}${d.namespace ? ' · ns: ' + esc(d.namespace) : ''}</div>
      ${d.detail ? `<div class="sdetail">${esc(d.detail)}</div>` : ''}
      ${chips ? `<div class="chips">${chips}</div>` : ''}
    </div></li>` +
    ((d.warnings || []).length
      ? `<li class="fail"><span class="mark">⚠</span><div>${warningsHTML(d.warnings, d.id)}</div></li>`
      : '') +
    (state.selTruncated
      ? `<li class="muted"><span class="mark"></span><div>+${esc(state.selTruncated)} more in this chain (highlight capped)</div></li>`
      : '');
  scheduleCardSync();
}

// trafficChain returns the full upstream + downstream chain of a node along
// traffic edges (WTGraph.chainOf, B-002): namespace and config edges are
// not walked, compound membership is not an edge at all.
function trafficChain(cy, id, opts) {
  const edges = cy.edges().map((e) => e.data()).filter(WTGraph.isTrafficEdge);
  return WTGraph.chainOf(id, edges, opts);
}

// highlightNeighborhood lights the clicked node with its whole traffic
// chain and dims everything else (B-002, replaces the direct neighbours of
// D-017). Namespaces only inspect: nothing dims.
function highlightNeighborhood(id) {
  const cy = state.cy;
  state.selNeighbors = new Set();
  state.selTruncated = 0;
  if (!cy) return;
  cy.elements().removeClass('focused dimmed');
  if (!id) return;
  const node = cy.getElementById(id);
  if (!node.length) return;
  if (isBox(node)) {
    cy.nodes().forEach((n) => state.selNeighbors.add(n.id()));
    return;
  }
  const chain = trafficChain(cy, id);
  state.selTruncated = chain.truncated;
  const keep = cy.collection();
  for (const n of chain.nodes) keep.merge(cy.getElementById(n));
  for (const e of chain.edges) keep.merge(cy.getElementById(e));
  // The boxes around the chain stay readable.
  keep.merge(keep.nodes().parent());
  keep.addClass('focused');
  cy.elements().not(keep).addClass('dimmed');
  keep.nodes().forEach((n) => state.selNeighbors.add(n.id()));
}

const MODAL_MAX_ZOOM = 0.7;

// openConnections renders the node's full traffic chain as a directed
// layered graph inside a modal (D-021/B-002) plus the warnings of every
// node in the chain, grouped by node.
function openConnections(id) {
  const cy = state.cy;
  if (!cy) return;
  const node = cy.getElementById(id);
  if (!node.length) return;
  const d = node.data();
  state.modalFocus = id;

  const chain = trafficChain(cy, id);
  const ordered = [...chain.nodes].sort((a, b) => chain.dist.get(a) - chain.dist.get(b));
  const els = [];
  const warnGroups = [];
  for (const nid of ordered) {
    const nd = Object.assign({}, cy.getElementById(nid).data());
    delete nd.parent; // no compound boxes in the modal
    els.push({ data: nd });
    if ((nd.warnings || []).length) warnGroups.push(nd);
  }
  for (const eid of chain.edges) {
    const e = cy.getElementById(eid);
    if (!chain.nodes.has(e.data('source')) || !chain.nodes.has(e.data('target'))) continue;
    els.push({ data: Object.assign({}, e.data()) });
  }

  const km = KIND_META[d.kind] || { title: d.kind };
  $('#modal-title').textContent = `${km.title}: ${d.label || ''} — chain of ${chain.nodes.size}` +
    (chain.truncated ? ` (+${chain.truncated} more)` : '');
  const mw = $('#modal-warnings');
  mw.innerHTML = warnGroups.length
    ? '<h2>warnings</h2>' + warnGroups.map((n) => {
      const nk = KIND_META[n.kind] || { title: n.kind };
      return `<div class="conn-group">${esc(nk.title)} · ${esc(n.namespace ? n.namespace + '/' : '')}${esc(n.label)}</div>${warningsHTML(n.warnings, 'modal|' + n.id)}`;
    }).join('')
    : '';
  mw.classList.toggle('hidden', !warnGroups.length);
  $('#modal').classList.remove('hidden');

  if (state.modalCy) { state.modalCy.destroy(); state.modalCy = null; }
  state.modalCards.clear();
  $('#modal-labels').innerHTML = '';

  state.modalCy = cytoscape({
    container: $('#modal-cy'),
    elements: els,
    layout: { name: 'dagre', rankDir: 'LR', nodeSep: 40, rankSep: 90, animate: false, fit: true, padding: 40 },
    wheelSensitivity: 0.2,
    autoungrabify: true,
    style: graphStyle(),
  });
  state.modalCy.on('pan zoom resize viewport', scheduleModalCardSync);
  state.modalCy.on('position', 'node', scheduleModalCardSync);
  state.modalCy.fit(undefined, 40);
  // A one- or two-card chain would be fitted to giant cards.
  if (state.modalCy.zoom() > MODAL_MAX_ZOOM) {
    state.modalCy.zoom(MODAL_MAX_ZOOM);
    state.modalCy.center();
  }
  scheduleModalCardSync();
  startFlow();
}

let modalCardScheduled = false;

function scheduleModalCardSync() {
  if (modalCardScheduled) return;
  modalCardScheduled = true;
  requestAnimationFrame(() => {
    modalCardScheduled = false;
    syncModalCards();
  });
}

function syncModalCards() {
  const cy = state.modalCy;
  if (!cy) return;
  const layer = $('#modal-labels');
  const zoom = cy.zoom();
  const compact = zoom < 0.45;

  for (const [id, div] of state.modalCards) {
    if (!cy.getElementById(id).length) {
      div.remove();
      state.modalCards.delete(id);
    }
  }

  cy.nodes().forEach((node) => {
    const d = node.data();
    const id = d.id;
    let div = state.modalCards.get(id);
    if (!div) {
      div = document.createElement('div');
      div.addEventListener('click', () => {
        const live = state.cy ? state.cy.getElementById(id) : null;
        if (live && live.length) inspectNode(live.data());
        closeModal();
      });
      layer.appendChild(div);
      state.modalCards.set(id, div);
    }
    div.className = 'node-card k-' + styleKind(d.kind) +
      (d.ok === false ? ' bad' : '') + (compact ? ' compact' : '') +
      (id === state.modalFocus ? ' sel' : '');
    const html = cardHTML(d, false);
    if (div._html !== html) { div.innerHTML = html; div._html = html; }
    div.style.width = nodeWidth(d.kind) + 'px';
    div.style.height = 'auto';
    div.style.transform = `translate(-50%, -50%) scale(${zoom})`;
    const rp = node.renderedPosition();
    div.style.left = rp.x + 'px';
    div.style.top = rp.y + 'px';
    if (compact) return;
    const measured = Math.max(div.offsetHeight, cardHeight(d));
    div.style.height = measured + 'px';
    if (Math.abs(node.height() - measured) > 0.5) {
      node.style('height', measured);
    }
  });
}

function closeModal() {
  $('#modal').classList.add('hidden');
  if (state.modalCy) { flowEdges.delete(state.modalCy); state.modalCy.destroy(); state.modalCy = null; }
  state.modalCards.clear();
  $('#modal-labels').innerHTML = '';
  state.modalFocus = null;
}

$('#modal-close').addEventListener('click', closeModal);
$('#modal').addEventListener('click', (ev) => { if (ev.target.id === 'modal') closeModal(); });
// Esc closes the innermost layer: modal, then the search dropdown, then the
// Warnings panel (D-040).
window.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape') return;
  if (!$('#modal').classList.contains('hidden')) { closeModal(); return; }
  if (state.search.open) { closeSearch(); return; }
  if (state.warnOpen) setWarnOpen(false);
});

// --- overview helpers ----------------------------------------------------

function fillNsFilter() {
  if (!state.clusterGraph) return;
  const sel = $('#ns-filter');
  const current = sel.value;
  const nss = [''];
  for (const n of state.clusterGraph.nodes) {
    if (n.kind === 'namespace' && !nss.includes(n.label)) nss.push(n.label);
  }
  const want = nss.join('|');
  if (sel.dataset.opts === want) return; // avoid clobbering an open dropdown
  sel.dataset.opts = want;
  sel.innerHTML = nss
    .map((ns) => `<option value="${esc(ns)}">${ns === '' ? 'all namespaces' : esc(ns)}</option>`)
    .join('');
  sel.value = nss.includes(current) ? current : '';
  state.nsFilter = sel.value;
}

function filteredGraph() {
  const g = state.clusterGraph;
  if (!g) return null;
  if (!state.nsFilter) return g;
  const keep = new Set();
  const nodes = [];
  for (const n of g.nodes) {
    if ((n.kind === 'namespace' && n.label === state.nsFilter) || n.namespace === state.nsFilter) {
      keep.add(n.id);
      nodes.push(n);
    }
  }
  const edges = g.edges.filter((e) => keep.has(e.from) && keep.has(e.to));
  return { nodes, edges };
}

function renderMapStats(g) {
  const el = $('#map-stats');
  if (!g) { el.innerHTML = ''; return; }
  const counts = {};
  for (const n of g.nodes) counts[n.kind] = (counts[n.kind] || 0) + 1;
  const row = (k) => `<div>${esc(k)}: <b>${esc(counts[k] || 0)}</b></div>`;
  el.innerHTML = row('namespace') + row('deployment') + row('statefulset') + row('daemonset') +
    row('service') + row('pod') + row('ingress') + row('ingressclass') + row('gateway') + row('virtualservice') +
    `<div>edges: <b>${esc(g.edges.length)}</b></div>`;
}

// --- websocket ----------------------------------------------------------

function connect() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/api/v1/stream`);
  ws.onopen = () => setConn(true);
  ws.onclose = () => { setConn(false); setTimeout(connect, 1500); };
  ws.onerror = () => ws.close();
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.type === 'sync') {
      state.checks = msg.checks;
      renderList();
      renderFormOptions();
      if (state.page === 'connectivity') render();
    } else if (msg.type === 'topology') {
      state.clusterGraph = msg.topology;
      fillNsFilter();
      renderFormOptions();
      refreshHosts(false);
      if (state.page === 'overview') render();
      // D-040 surfaces follow every sync without touching focus, scroll or
      // open lists.
      renderWarnings();
      refreshSearch();
    }
  };
}

// --- flowing traffic edges (D-039) ----------------------------------------

const FLOW_DASH = [8, 6];
const FLOW_PERIOD = FLOW_DASH[0] + FLOW_DASH[1];
const FLOW_SPEED = 24;   // model px/s (the dash pattern is in model px too)
const FLOW_MIN_MS = 33;  // ~30 fps
const reducedMotion = window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : null;
const flowEdges = new WeakMap(); // cy instance → collection of [?flow] edges

// flowEnabled: flow is on and the user did not ask for reduced motion.
function flowEnabled() {
  return state.flowOn && !(reducedMotion && reducedMotion.matches);
}

function flowEdgesOf(cy) {
  let eles = flowEdges.get(cy);
  if (!eles) {
    eles = cy.edges('[?flow]');
    flowEdges.set(cy, eles);
  }
  return eles;
}

// startFlow runs the single animation loop; it stops by itself while the
// tab is hidden or flow is off (visibilitychange/toggle restart it).
function startFlow() {
  if (state.flowRaf || !flowEnabled() || document.hidden) return;
  state.flowRaf = requestAnimationFrame(flowTick);
}

function flowTick(ts) {
  state.flowRaf = 0;
  if (!flowEnabled() || document.hidden) return;
  state.flowRaf = requestAnimationFrame(flowTick);
  if (ts - state.flowLast < FLOW_MIN_MS) return;
  state.flowLast = ts;
  const t0 = performance.now();
  // The offset decreases: the pattern moves from source to target.
  const offset = FLOW_PERIOD - ((ts / 1000 * FLOW_SPEED) % FLOW_PERIOD);
  // Far overview: cards are hidden and dashes invisible, skip the work.
  const main = state.cy && !(state.page === 'overview' && state.cy.zoom() < 0.35) ? state.cy : null;
  for (const cy of [main, state.modalCy]) {
    if (!cy) continue;
    const eles = flowEdgesOf(cy);
    if (eles.length) eles.style('line-dash-offset', offset);
  }
  const st = state.flowStats;
  const dt = performance.now() - t0;
  st.frames++;
  st.totalMs += dt;
  st.maxMs = Math.max(st.maxMs, dt);
}

// applyFlowStyle re-applies the stylesheet (dashed vs solid traffic edges)
// and updates the toggle after a flow setting changed.
function applyFlowStyle() {
  $('#flow-btn').textContent = 'flow ' + (state.flowOn ? 'on' : 'off');
  for (const cy of [state.cy, state.modalCy]) {
    if (!cy) continue;
    flowEdges.delete(cy);
    cy.style(graphStyle());
  }
  startFlow();
}

document.addEventListener('visibilitychange', startFlow);
if (reducedMotion && reducedMotion.addEventListener) {
  reducedMotion.addEventListener('change', applyFlowStyle);
}

// --- connectivity page (D-013) -------------------------------------------

function checkKey(c) { return c.namespace + '/' + c.name; }

function currentCheck() {
  return state.checks.find((c) => checkKey(c) === state.selected) || null;
}

function badgeClass(state) {
  if (state === 'Passed') return 'ok';
  if (state === 'Failed') return 'fail';
  return 'pending';
}

function renderList() {
  const ul = $('#check-list');
  ul.innerHTML = '';
  for (const c of state.checks) {
    const li = document.createElement('li');
    li.className = 'check' + (checkKey(c) === state.selected ? ' active' : '');
    li.innerHTML = `
      <span class="badge ${badgeClass(c.state)}">${esc(c.state || 'Pending')}</span>
      <span class="cname" title="${esc(checkKey(c))}">${esc(c.name)}</span>
      <span class="ns">${esc(c.namespace)}</span>
      <button class="del" title="delete">×</button>`;
    li.addEventListener('click', () => {
      state.selected = checkKey(c);
      renderList();
      render();
    });
    li.querySelector('.del').addEventListener('click', async (ev) => {
      ev.stopPropagation();
      await fetch(`/api/v1/checks/${encodeURIComponent(c.namespace)}/${encodeURIComponent(c.name)}`, { method: 'DELETE' });
    });
    ul.appendChild(li);
  }
}

function renderSteps(c) {
  const ol = $('#step-list');
  ol.innerHTML = '';
  if (!c || !c.steps || c.steps.length === 0) {
    ol.innerHTML = '<li class="muted">no steps yet — waiting for the first reconcile…</li>';
    return;
  }
  for (const s of c.steps) {
    const li = document.createElement('li');
    li.className = s.ok ? 'ok' : 'fail';
    li.innerHTML = `
      <span class="mark">${s.ok ? '✓' : '✗'}</span>
      <div>
        <div class="sname">${esc(s.name)}</div>
        ${s.detail ? `<div class="sdetail">${esc(s.detail)}</div>` : ''}
        ${!s.ok && s.hint ? `<div class="shint">→ ${esc(s.hint)}</div>` : ''}
      </div>`;
    ol.appendChild(li);
  }
}

// --- upstream north-south chain of a workload check (B-001) ----------------

const UPSTREAM_TTL_MS = 30000;

function upstreamKey(c) {
  return `${c.namespace}|${c.serviceName || ''}|${c.target}`;
}

// refreshUpstream fetches the routes reaching the check's Service (cached
// per check, at most every UPSTREAM_TTL_MS) and re-renders when they land.
async function refreshUpstream(c) {
  const key = upstreamKey(c);
  const prev = state.upstream;
  const same = prev && prev.key === key;
  if (same && (prev.loading || Date.now() - prev.at < UPSTREAM_TTL_MS)) return;
  state.upstream = { key, data: same ? prev.data : null, error: '', loading: true, at: Date.now() };
  const qs = new URLSearchParams({ namespace: c.namespace });
  if (c.serviceName) qs.set('service', c.serviceName); else qs.set('target', c.target);
  let data = null;
  let error = '';
  try {
    const res = await fetch(`/api/v1/upstream?${qs}`);
    const body = await res.json().catch(() => ({}));
    if (res.ok) data = body; else error = body.error || `upstream failed (${res.status})`;
  } catch (err) {
    error = err.message;
  }
  if (!state.upstream || state.upstream.key !== key) return; // selection moved on
  state.upstream = { key, data: data || (same ? prev.data : null), error, loading: false, at: Date.now() };
  // First answer for this check: fit the longer graph once (user-visible
  // context change, like switching checks).
  if (!same && data && (data.hosts || []).length) state.fitDone = false;
  const cur = currentCheck();
  if (state.page === 'connectivity' && state.connMode === 'workload' && cur && upstreamKey(cur) === key) render();
}

// mergeUpstream returns the check topology with the north-south chain on
// top of its Service. Trace node IDs are graph IDs while the check topology
// uses the local id "svc" for its Service: that one is aliased so the chain
// lands on the existing node. Backend hops below the Service come from the
// check itself; host/address hops are left out (outside the cluster).
function mergeUpstream(c) {
  const topo = { nodes: (c.topology.nodes || []).slice(), edges: (c.topology.edges || []).slice() };
  const u = state.upstream;
  const up = u && u.key === upstreamKey(c) ? u.data : null;
  if (!up || !(up.traces || []).length) {
    topo.edges = pruneEdges(topo.edges, new Set(topo.nodes.map((n) => n.id)), 'mergeUpstream');
    return topo;
  }

  const nodeIDs = new Map(topo.nodes.map((n) => [n.id, n]));
  const svc = nodeIDs.get('svc');
  const alias = new Map();
  if (svc) alias.set(`svc:${up.namespace}/${String(svc.label || '').split('\n')[0]}`, 'svc');
  const backendIDs = (up.services || []).map((name) => `svc:${up.namespace}/${name}`);
  const edgeKeys = new Set(topo.edges.map((e) => `${e.from}→${e.to}`));
  const mapID = (id) => alias.get(id) || id;

  for (const t of up.traces) {
    const tn = (t.topology || {}).nodes || [];
    const te = (t.topology || {}).edges || [];
    // Everything downstream of the Service is dropped.
    const drop = new Set();
    const stack = backendIDs.filter((id) => tn.some((n) => n.id === id));
    while (stack.length) {
      const id = stack.pop();
      for (const e of te) {
        if (e.from === id && !drop.has(e.to)) {
          drop.add(e.to);
          stack.push(e.to);
        }
      }
    }
    const outside = new Set(tn.filter((n) => TRACE_KINDS.has(n.kind)).map((n) => n.id));
    for (const n of tn) {
      if (drop.has(n.id) || outside.has(n.id) || nodeIDs.has(mapID(n.id))) continue;
      nodeIDs.set(n.id, n);
      topo.nodes.push(n);
    }
    for (const e of te) {
      if (drop.has(e.from) || drop.has(e.to) || outside.has(e.from) || outside.has(e.to)) continue;
      const key = `${mapID(e.from)}→${mapID(e.to)}`;
      if (edgeKeys.has(key)) continue;
      edgeKeys.add(key);
      topo.edges.push(Object.assign({}, e, { from: mapID(e.from), to: mapID(e.to) }));
    }
  }

  // A claim whose trace does not reach the Service still points at it: draw
  // the broken link so the failed branch ends where the user is looking.
  for (const h of up.hosts || []) {
    if (h.reaches || !h.route || !nodeIDs.has(h.route)) continue;
    const to = mapID(`svc:${up.namespace}/${h.service}`);
    const key = `${h.route}→${to}`;
    if (!nodeIDs.has(to) || edgeKeys.has(key)) continue;
    edgeKeys.add(key);
    topo.edges.push({ from: h.route, to, label: 'no path', ok: false });
  }

  // The check's generic client → Service request moves to the chain head.
  if (svc && edgeKeys.has('client→svc')) {
    const hasIn = new Set(topo.edges.map((e) => e.to));
    const heads = topo.nodes.filter((n) => n.id !== 'client' && !hasIn.has(n.id));
    if (heads.length) {
      topo.edges = topo.edges.filter((e) => !(e.from === 'client' && e.to === 'svc'));
      for (const n of heads) topo.edges.push({ from: 'client', to: n.id, label: 'request' });
    }
  }
  topo.edges = pruneEdges(topo.edges, new Set(topo.nodes.map((n) => n.id)), 'mergeUpstream');
  return topo;
}

// renderWorkloadSteps renders the check's steps followed by the upstream
// groups (B-001): one group per host, same step → finding → hint style as
// the host trace; unknown/skipped stay muted.
function renderWorkloadSteps(c) {
  renderSteps(c);
  const u = state.upstream;
  if (!c || !u || u.key !== upstreamKey(c)) return;
  const head = (text) => `<li class="branch-head"><span class="mark"></span><div><div class="sname">${text}</div></div></li>`;
  const muted = (text) => `<li class="muted"><span class="mark"></span><div>${esc(text)}</div></li>`;
  let html = head('Upstream (north-south)');
  const hosts = u.data ? u.data.hosts || [] : [];
  if (!u.data && u.loading) {
    html += muted('loading routes…');
  } else if (!u.data) {
    html += muted(`upstream unavailable: ${u.error || 'unknown error'}`);
  } else if (!hosts.length) {
    html += muted('no ingress route reaches this service');
  }
  for (const h of hosts) {
    const t = h.trace >= 0 ? u.data.traces[h.trace] : null;
    const badge = t ? `<span class="badge ${traceBadge(t.state)}">${esc(t.state)}</span> ` : '';
    const via = [h.provider, h.route].filter(Boolean).join(' · ');
    html += head(`${badge}${esc(h.host)}${esc(h.path)}` + (via ? ` <span class="ns">${esc(via)}</span>` : ''));
    if (!t) {
      html += muted(h.note || 'not traced');
      continue;
    }
    const branches = t.branches || [];
    if (!branches.length) html += muted('no branch of this trace reaches the service');
    branches.forEach((b, i) => {
      if (branches.length > 1) html += muted(`branch ${i + 1}: ${b.provider} · ${b.entry} (${b.state})`);
      html += (b.steps || []).filter((s) => s.hop !== 'outside').map(traceStepHTML).join('');
    });
  }
  if (u.data && u.data.truncated) html += muted('more routes exist than were traced (cap reached)');
  $('#step-list').insertAdjacentHTML('beforeend', html);
}

// --- form options (D-014: dropdowns + autocomplete) -----------------------

function renderFormOptions() {
  const g = state.clusterGraph;
  if (!g) return;

  const nsSel = $('#check-ns');
  const current = nsSel.value || 'default';
  const nss = new Set(['default']);
  for (const n of g.nodes) if (n.namespace) nss.add(n.namespace);
  const opts = [...nss].sort().join('|');
  if (nsSel.dataset.opts !== opts) {
    nsSel.dataset.opts = opts;
    nsSel.innerHTML = [...nss].sort().map((ns) => `<option value="${esc(ns)}">${esc(ns)}</option>`).join('');
  }
  nsSel.value = nss.has(current) ? current : 'default';
  fillWorkloadOptions(false);
}

// fillWorkloadOptions rebuilds the target/service selects. Targets carry
// their kind in the label (D-017); on namespace change both are cleared.
function fillWorkloadOptions(reset) {
  const g = state.clusterGraph;
  if (!g) return;
  const ns = $('#check-ns').value;
  const tSel = $('#check-target');
  const sSel = $('#check-service');
  const prevT = tSel.value;
  const prevS = sSel.value;

  const targets = [];
  const services = [];
  for (const n of g.nodes) {
    if (n.namespace !== ns) continue;
    if (WORKLOAD_KINDS.has(n.kind) || n.kind === 'pod') targets.push({ name: n.label, kind: n.kind });
    if (n.kind === 'service') services.push(n.label);
  }
  targets.sort((a, b) => a.name.localeCompare(b.name));
  services.sort();

  tSel.innerHTML = '<option value="">— pick a workload —</option>' +
    targets.map((t) => `<option value="${esc(t.name)}" data-kind="${esc(t.kind)}">${esc(t.kind)} · ${esc(t.name)}</option>`).join('');
  sSel.innerHTML = '<option value="">— none (use target) —</option>' +
    services.map((svc) => `<option value="${esc(svc)}">${esc(svc)}</option>`).join('');

  tSel.value = (!reset && targets.some((t) => t.name === prevT)) ? prevT : '';
  sSel.value = (!reset && services.includes(prevS)) ? prevS : '';
}

$('#check-ns').addEventListener('change', () => fillWorkloadOptions(true));
$('#check-target').addEventListener('change', () => { $('#check-service').value = ''; });

$('#ns-filter').addEventListener('change', (ev) => {
  state.nsFilter = ev.target.value;
  // Explicit user action: refit is expected here.
  resetOverviewGraph();
});

$('#fit-btn').addEventListener('click', () => {
  if (!state.cy) return;
  state.cy.fit(undefined, 60);
  scheduleCardSync();
});

$('#flow-btn').addEventListener('click', () => {
  state.flowOn = !state.flowOn;
  try {
    localStorage.setItem(FLOW_KEY, state.flowOn ? '1' : '0');
  } catch {
    // No storage: the toggle still works for this page load.
  }
  applyFlowStyle();
});

// --- new check form -------------------------------------------------------

$('#new-check').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const form = ev.target;
  const body = {
    namespace: $('#check-ns').value || 'default',
    target: $('#check-target').value,
    serviceName: $('#check-service').value,
    port: form.port.value ? Number(form.port.value) : 0,
  };
  if (!body.target) {
    $('#form-error').textContent = 'pick a target workload';
    return;
  }
  const err = $('#form-error');
  err.textContent = '';
  const res = await fetch('/api/v1/checks', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    err.textContent = `request failed (${res.status})`;
    return;
  }
  const created = await res.json();
  state.selected = checkKey(created);
  switchPage('connectivity');
  $('#check-target').value = '';
  $('#check-service').value = '';
  form.port.value = '';
  renderList();
});

// --- host trace (D-038) ---------------------------------------------------

// runTrace queries the host trace. The host input may carry
// "[scheme://]host[:port][/path]"; the optional test path field wins over a
// pasted path when both are set and differ (WTGraph.traceQuery, D-040). A
// URL scheme is sent as the scheme param (the explicit selector wins,
// D-038a). Only route configuration is matched: no request is ever sent.
async function runTrace(rawHost, rawPath, scheme) {
  const parsed = WTGraph.traceQuery(rawHost, rawPath);
  if (!parsed.host) throw new Error('enter a host');
  const qs = new URLSearchParams({ host: parsed.host, path: parsed.path });
  const sch = scheme || parsed.scheme;
  if (sch) qs.set('scheme', sch);
  const res = await fetch(`/api/v1/trace?${qs}`);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `trace failed (${res.status})`);
  return body;
}

// refreshHosts reloads the host autocomplete (at most every 10s unless forced).
async function refreshHosts(force) {
  const now = Date.now();
  if (!force && now - state.hostsFetchedAt < 10000) return;
  state.hostsFetchedAt = now;
  try {
    const res = await fetch('/api/v1/hosts');
    if (!res.ok) return;
    const hosts = await res.json();
    $('#host-list').innerHTML = hosts
      .map((h) => `<option value="${esc(h.host)}">${esc((h.sources || []).join(', '))}</option>`)
      .join('');
  } catch {
    // autocomplete is optional
  }
}

function traceBadge(st) {
  if (st === 'Passed') return 'ok';
  if (st === 'Failed') return 'fail';
  // Warning (D-040: route configured, path not served) shares the warn
  // badge with Partial — no new colours.
  if (st === 'Partial' || st === 'Warning') return 'warn';
  return 'pending';
}

function stepClass(status) {
  if (status === 'ok') return 'ok';
  if (status === 'fail') return 'fail';
  if (status === 'warn') return 'warn';
  return 'muted';
}

function stepMark(status) {
  return { ok: '✓', fail: '✗', warn: '⚠', unknown: '?', skipped: '–' }[status] || '·';
}

function traceStepHTML(s) {
  const reason = s.reason ? ` <span class="ns">${esc(s.reason)}</span>` : '';
  return `<li class="${stepClass(s.status)}"><span class="mark">${stepMark(s.status)}</span><div>` +
    (s.group ? `<div class="sdetail">↳ ${esc(s.group)}</div>` : '') +
    `<div class="sname">${esc(s.name)}${reason}</div>` +
    (s.detail ? `<div class="sdetail">${esc(s.detail)}</div>` : '') +
    itemsHTML(s.items, `step|${s.hop}|${s.ref || ''}|${s.reason || ''}|${s.group || ''}|${s.name}`,
      s.reason === 'pathNotRouted' ? 'path' : 'host') +
    (s.status !== 'ok' && s.hint ? `<div class="shint">→ ${esc(s.hint)}</div>` : '') +
    '</div></li>';
}

// withStepWarnings returns the trace topology nodes with every warn step
// attached to its node as a card warning (D-040: e.g. pathNotRouted on the
// VirtualService), so the graph shows where the request stops being served.
function withStepWarnings(t) {
  const extra = new Map();
  for (const b of t.branches || []) {
    for (const st of b.steps || []) {
      if (st.status !== 'warn' || !st.ref) continue;
      const list = extra.get(st.ref) || [];
      const reason = st.reason || 'warn';
      if (!list.some((w) => w.reason === reason)) {
        list.push({ reason, message: st.detail || st.name, hint: st.hint || '', items: st.items || [] });
      }
      extra.set(st.ref, list);
    }
  }
  return (t.topology.nodes || []).map((n) => {
    const add = extra.get(n.id);
    if (!add) return n;
    const have = new Set((n.warnings || []).map((w) => w.reason));
    return Object.assign({}, n, { warnings: (n.warnings || []).concat(add.filter((w) => !have.has(w.reason))) });
  });
}

// renderTraceSteps renders request-level steps, then every branch as
// step → finding → hint (same style as check steps; unknown/skipped muted).
function renderTraceSteps(t) {
  const ol = $('#step-list');
  if (!t) {
    ol.innerHTML = '<li class="muted">no trace yet — enter a host (e.g. from the autocomplete)</li>';
    return;
  }
  const r = t.request || {};
  let html = `<li class="branch-head"><span class="mark"></span><div><div class="sname">` +
    `<span class="badge ${traceBadge(t.state)}">${esc(t.state)}</span> ${esc(r.scheme)}://${esc(r.host)}:${esc(r.port)}${esc(r.path)}</div></div></li>`;
  html += (t.steps || []).map(traceStepHTML).join('');
  (t.branches || []).forEach((b, i) => {
    html += `<li class="branch-head"><span class="mark"></span><div><div class="sname">` +
      `<span class="badge ${traceBadge(b.state)}">${esc(b.state)}</span> branch ${i + 1}: ${esc(b.provider)} · ${esc(b.entry)}</div></div></li>`;
    html += (b.steps || []).filter((s) => s.hop !== 'outside').map(traceStepHTML).join('');
  });
  ol.innerHTML = html;
}

// renderBranchList fills the Connectivity sidebar with the trace branches;
// clicking one highlights its path in the graph.
function renderBranchList() {
  const ul = $('#branch-list');
  const t = state.trace;
  if (!t) { ul.innerHTML = ''; return; }
  if (!(t.branches || []).length) {
    ul.innerHTML = `<li class="check"><span class="badge ${traceBadge(t.state)}">${esc(t.state)}</span><span class="cname">no entry point</span></li>`;
    return;
  }
  ul.innerHTML = '';
  t.branches.forEach((b, i) => {
    const li = document.createElement('li');
    li.className = 'check' + (state.traceBranch === i ? ' active' : '');
    li.innerHTML = `<span class="badge ${traceBadge(b.state)}">${esc(b.state)}</span>` +
      `<span class="cname" title="${esc(b.entry)}">${esc(b.provider)} · ${esc(b.entry)}</span>`;
    li.addEventListener('click', () => {
      state.traceBranch = state.traceBranch === i ? -1 : i;
      renderBranchList();
      highlightSet(state.traceBranch >= 0 ? branchHL(t, state.traceBranch) : null);
    });
    ul.appendChild(li);
  });
}

// branchHL builds a highlight set from one branch (or all with i < 0).
function branchHL(t, i) {
  const nodes = new Set();
  const edges = new Set();
  (t.branches || []).forEach((b, j) => {
    if (i >= 0 && i !== j) return;
    for (const n of (b.highlight || {}).nodes || []) nodes.add(n);
    for (const e of (b.highlight || {}).edges || []) edges.add(`${e.from}→${e.to}`);
  });
  return { nodes, edges };
}

// highlightSet focuses the given nodes/edges and dims the rest — the same
// focused/dimmed mechanism as the neighborhood highlight (D-017).
function highlightSet(hl) {
  const cy = state.cy;
  if (!cy) return;
  cy.elements().removeClass('focused dimmed');
  state.selNeighbors = new Set();
  if (!hl) { scheduleCardSync(); return; }
  const keep = cy.collection();
  cy.nodes().forEach((n) => { if (hl.nodes.has(n.id())) keep.merge(n); });
  cy.edges().forEach((e) => {
    if (hl.edges.has(`${e.source().id()}→${e.target().id()}`)) keep.merge(e);
  });
  keep.merge(keep.nodes().parent());
  keep.addClass('focused');
  cy.elements().not(keep).addClass('dimmed');
  keep.nodes().forEach((n) => state.selNeighbors.add(n.id()));
  scheduleCardSync();
}

// applyTraceHighlight re-applies the overview trace highlight (after a
// background sync or when the node selection is cleared).
function applyTraceHighlight() {
  if (state.page === 'overview' && state.traceHL && !state.selNode) {
    highlightSet(state.traceHL);
  } else if (state.page === 'connectivity' && state.connMode === 'host' && state.trace && state.traceBranch >= 0 && !state.selNode) {
    highlightSet(branchHL(state.trace, state.traceBranch));
  }
}

// fitTraceHighlight zooms the overview to the highlighted path so it is
// visible right after a search (explicit user action: moving the viewport
// is expected here, unlike background syncs — D-016).
function fitTraceHighlight() {
  const cy = state.cy;
  if (!cy || !state.traceHL) return;
  // Compound boxes span whole namespaces: fit the path, not its boxes.
  const els = cy.elements('.focused').not('node[kind = "namespace"]');
  if (!els.length) return;
  cy.fit(els, 80);
  // A path spanning far-apart namespace cells would fit below the
  // far-zoom threshold (cards hidden): keep cards readable and centre on
  // the route (VirtualService / Ingress) — the backend sits in its cell.
  if (cy.zoom() < 0.45) {
    const nodes = els.nodes();
    const route = nodes.filter((n) => ['virtualservice', 'ingress'].includes(n.data('kind')));
    cy.zoom(0.45);
    cy.center(route.length ? route.union(route.outgoers().nodes().filter((n) => els.contains(n))) : nodes);
  }
  scheduleCardSync();
}

// renderOverviewTrace shows the trace summary under the overview search box.
function renderOverviewTrace() {
  const el = $('#ov-trace');
  const t = state.trace;
  if (!t || !state.traceHL) { el.innerHTML = ''; return; }
  const r = t.request || {};
  const visible = [...state.traceHL.nodes].filter((id) => state.cy && state.cy.getElementById(id).length).length;
  let html = `<div><span class="badge ${traceBadge(t.state)}">${esc(t.state)}</span> ${esc(r.host)}${esc(r.path)}</div>`;
  const branches = t.branches || [];
  if (branches.length > 1) {
    html += branches.map((b, i) =>
      `<div><label><input type="radio" name="ov-branch" value="${i}"${state.traceBranch === i ? ' checked' : ''}> ` +
      `${esc(b.provider)} · ${esc(b.entry)} (${esc(b.state)})</label></div>`).join('') +
      `<div><label><input type="radio" name="ov-branch" value="-1"${state.traceBranch < 0 ? ' checked' : ''}> all branches</label></div>`;
  } else if (branches.length === 1) {
    html += `<div>${esc(branches[0].provider)} · ${esc(branches[0].entry)}</div>`;
  } else {
    html += '<div>no entry point claims this host</div>';
  }
  html += `<div>highlighted: <b>${visible}</b> cards</div>`;
  if (state.clusterGraph && t.graphVersion !== state.clusterGraph.version) {
    html += `<div class="error">map changed since this trace — <a href="#" id="ov-trace-refresh">refresh</a></div>`;
  }
  html += '<button id="ov-trace-open" class="ghost-btn">open in Connectivity</button>' +
    '<button id="ov-trace-clear" class="ghost-btn">clear highlight</button>';
  el.innerHTML = html;
  el.querySelectorAll('input[name="ov-branch"]').forEach((inp) => inp.addEventListener('change', () => {
    state.traceBranch = Number(inp.value);
    state.traceHL = branchHL(t, state.traceBranch);
    applyTraceHighlight();
    fitTraceHighlight();
    renderOverviewTrace();
  }));
  $('#ov-trace-open').addEventListener('click', () => {
    state.connMode = 'host';
    setConnMode('host');
    $('#trace-host').value = r.host || '';
    $('#trace-path').value = r.path && r.path !== '/' ? r.path : '';
    switchPage('connectivity');
  });
  $('#ov-trace-clear').addEventListener('click', () => {
    state.traceHL = null;
    // Keep a selected node's chain lit; with no selection this clears all.
    highlightNeighborhood(state.selNode);
    renderOverviewTrace();
    scheduleCardSync();
  });
  const refresh = $('#ov-trace-refresh');
  if (refresh) {
    refresh.addEventListener('click', (ev) => {
      ev.preventDefault();
      $('#ov-trace-form').requestSubmit();
    });
  }
}

$('#ov-trace-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const el = $('#ov-trace');
  try {
    splitHostField($('#ov-host'), $('#ov-path'));
    const t = await runTrace($('#ov-host').value, $('#ov-path').value, '');
    state.trace = t;
    state.traceBranch = -1;
    state.traceHL = branchHL(t, -1);
    state.selNode = null;
    state.selNeighbors = new Set();
    setDetailsHint();
    applyTraceHighlight();
    fitTraceHighlight();
    renderOverviewTrace();
  } catch (err) {
    el.innerHTML = `<div class="error">${esc(err.message)}</div>`;
  }
});

$('#host-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const errEl = $('#trace-error');
  errEl.textContent = '';
  try {
    splitHostField($('#trace-host'), $('#trace-path'));
    state.trace = await runTrace($('#trace-host').value, $('#trace-path').value, $('#trace-scheme').value);
    state.traceBranch = -1;
    // New trace = new graph context: refit once.
    if (state.cy) { state.cy.destroy(); state.cy = null; }
    clearCards();
    state.selNode = null;
    state.fitDone = false;
    render();
  } catch (err) {
    errEl.textContent = err.message;
  }
});

// setConnMode toggles the Connectivity page between workload checks and
// host traces (D-038).
function setConnMode(mode) {
  state.connMode = mode;
  for (const b of document.querySelectorAll('#mode-toggle .tab')) {
    b.classList.toggle('active', b.dataset.mode === mode);
  }
  $('#conn-host').classList.toggle('hidden', mode !== 'host');
  $('#conn-workload').classList.toggle('hidden', mode !== 'workload');
}

document.querySelectorAll('#mode-toggle .tab').forEach((b) => b.addEventListener('click', () => {
  if (state.connMode === b.dataset.mode) return;
  setConnMode(b.dataset.mode);
  if (state.page === 'connectivity') switchPage('connectivity');
  if (b.dataset.mode === 'host') refreshHosts(true);
}));

// --- D-040: focus a node from the Warnings panel / global search ----------

// Readable zoom band for a focused node, like the host-search fit floor
// (fitTraceHighlight); the cap keeps a one-card chain from filling the
// screen.
// Cards switch to compact headers below 0.5, so a focused resource lands
// just above that (D-019 thresholds); a chain that fits higher keeps its fit.
const FOCUS_MIN_ZOOM = 0.6;
const FOCUS_MAX_ZOOM = 1;

// resetOverviewGraph rebuilds the Overview after a filter change (explicit
// user action: a refit is expected).
function resetOverviewGraph() {
  if (state.cy) { state.cy.destroy(); state.cy = null; }
  clearCards();
  state.selNode = null;
  state.selNeighbors = new Set();
  state.fitDone = false;
  render();
}

// focusNode switches to the Overview, clears a namespace filter that would
// hide the node (the full map stays the context), selects it (chain
// highlight + inspector) and brings it into view.
function focusNode(id) {
  const g = state.clusterGraph;
  const n = g && g.nodes.find((x) => x.id === id);
  if (!n) return;
  const hidden = !!state.nsFilter && n.namespace !== state.nsFilter &&
    !(n.kind === 'namespace' && n.label === state.nsFilter);
  if (hidden) {
    state.nsFilter = '';
    $('#ns-filter').value = '';
  }
  if (state.page !== 'overview') switchPage('overview');
  else if (hidden) resetOverviewGraph();
  const cy = state.cy;
  const node = cy ? cy.getElementById(id) : null;
  if (!node || !node.length) return;
  inspectNode(node.data());
  if (isBox(node)) {
    cy.fit(node, 60);
    if (cy.zoom() > FOCUS_MAX_ZOOM) { cy.zoom(FOCUS_MAX_ZOOM); cy.center(node); }
  } else {
    const chain = cy.elements('.focused').not('node[kind = "namespace"]');
    if (chain.nodes().length > 1) cy.fit(chain, 80);
    const z = cy.zoom();
    if (chain.nodes().length <= 1 || z < FOCUS_MIN_ZOOM || z > FOCUS_MAX_ZOOM) {
      cy.zoom(z < FOCUS_MIN_ZOOM || chain.nodes().length <= 1 ? 0.8 : Math.min(FOCUS_MAX_ZOOM, z));
      cy.center(node);
    }
  }
  scheduleCardSync();
}

// --- D-040: Warnings panel ---------------------------------------------------

function setWarnOpen(open) {
  state.warnOpen = open;
  $('#warn-panel').classList.toggle('hidden', !open);
  $('#warn-btn').classList.toggle('active', open);
  $('#warn-btn').setAttribute('aria-expanded', open ? 'true' : 'false');
  state.warnHTML = '';
  renderWarnings();
}

// fillWarnNs keeps the panel's namespace filter in sync with the namespaces
// that have warnings, without clobbering an open dropdown.
function fillWarnNs(groups) {
  const sel = $('#warn-ns');
  const current = sel.value;
  const nss = groups.map((g) => g.namespace).filter(Boolean);
  if (current && !nss.includes(current)) nss.push(current);
  nss.sort();
  const want = nss.join('|');
  if (sel.dataset.opts === want) return;
  sel.dataset.opts = want;
  sel.innerHTML = '<option value="">all namespaces</option>' +
    nss.map((ns) => `<option value="${esc(ns)}">${esc(ns)}</option>`).join('');
  sel.value = current;
}

// renderWarnings updates the top-bar count and, when open, the panel list.
// Background syncs re-render only when the list really changed; scroll,
// filter focus and open item lists (keyed by node id + reason) survive.
function renderWarnings() {
  const g = state.clusterGraph;
  const all = WTGraph.collectWarnings(g);
  const badge = $('#warn-count');
  badge.textContent = String(all.total);
  badge.className = 'badge ' + (all.total ? 'warn' : 'pending');
  fillWarnNs(all.groups);
  if (!state.warnOpen) return;
  const res = WTGraph.collectWarnings(g, { text: $('#warn-text').value, ns: $('#warn-ns').value });
  $('#warn-title').textContent = `warnings · ${res.shown}` + (res.shown !== res.total ? ` of ${res.total}` : '');
  let html = '';
  if (!g) html = '<div class="warn-empty">waiting for the topology mapper…</div>';
  else if (!res.total) html = '<div class="warn-empty">no warnings in the cluster map</div>';
  else if (!res.shown) html = '<div class="warn-empty">no warning matches the filter</div>';
  for (const grp of res.groups) {
    html += `<div class="conn-group">${grp.namespace ? esc(grp.namespace) : 'cluster-scoped'}</div>`;
    for (const r of grp.resources) {
      const km = KIND_META[r.kind] || { title: r.kind };
      html += `<div class="warn-res"><div class="warn-res-head">${esc(km.title)} · <b>${esc(r.label)}</b></div>`;
      for (const row of r.rows) {
        const broken = row.type === 'broken';
        html += `<div class="warn-row${broken ? ' broken' : ''}" data-id="${esc(r.id)}" tabindex="0" role="button">` +
          `<span class="mark">${broken ? '✗' : '⚠'}</span><div>` +
          `<div class="sname">${esc(row.message)}${broken ? '' : ` <span class="ns">${esc(row.reason)}</span>`}</div>` +
          itemsHTML(row.items, 'panel|' + row.key) +
          (row.hint ? `<div class="shint">→ ${esc(row.hint)}</div>` : '') +
          '</div></div>';
      }
      html += '</div>';
    }
  }
  if (html === state.warnHTML) return;
  const list = $('#warn-list');
  const top = list.scrollTop;
  list.innerHTML = html;
  list.scrollTop = top;
  state.warnHTML = html;
}

function warnRowTarget(ev) {
  if (ev.target.closest('.items')) return null; // the list toggle stays a toggle
  const row = ev.target.closest('.warn-row');
  return row ? row.dataset.id : null;
}

$('#warn-btn').addEventListener('click', () => setWarnOpen(!state.warnOpen));
$('#warn-close').addEventListener('click', () => setWarnOpen(false));
// A new filter is a new list: start at its top (syncs keep the scroll).
function onWarnFilter() {
  renderWarnings();
  $('#warn-list').scrollTop = 0;
}
$('#warn-text').addEventListener('input', onWarnFilter);
$('#warn-ns').addEventListener('change', onWarnFilter);
$('#warn-list').addEventListener('click', (ev) => {
  const id = warnRowTarget(ev);
  if (id) focusNode(id);
});
$('#warn-list').addEventListener('keydown', (ev) => {
  if (ev.key !== 'Enter' && ev.key !== ' ') return;
  const id = warnRowTarget(ev);
  if (!id) return;
  ev.preventDefault();
  focusNode(id);
});

// --- D-040: global resource search -------------------------------------------

const SEARCH_LIMIT = 12;

function closeSearch() {
  state.search.open = false;
  state.search.active = -1;
  $('#search-drop').classList.add('hidden');
  $('#res-search').setAttribute('aria-expanded', 'false');
}

// refreshSearch recomputes the results (typing, or a topology sync while
// the dropdown is open); the active row follows its node across syncs.
function refreshSearch(openIt) {
  const inp = $('#res-search');
  if (openIt) state.search.open = true;
  if (!state.search.open) return;
  if (!inp.value.trim() || !state.clusterGraph || state.page !== 'overview') { closeSearch(); return; }
  const prev = state.search.results[state.search.active];
  const r = WTGraph.searchNodes(state.clusterGraph.nodes, inp.value, SEARCH_LIMIT);
  state.search.results = r.results;
  state.search.total = r.total;
  const keep = prev ? r.results.findIndex((n) => n.id === prev.id) : -1;
  state.search.active = keep >= 0 ? keep : (r.results.length ? 0 : -1);
  renderSearchDrop();
}

function renderSearchDrop() {
  const drop = $('#search-drop');
  const { results, total, active } = state.search;
  let html = results.map((n, i) => {
    const km = KIND_META[n.kind] || { title: n.kind };
    const warns = (n.warnings || []).length;
    return `<div class="search-row${i === active ? ' active' : ''}" data-i="${i}" role="option" aria-selected="${i === active}">` +
      `<span class="s-kind">${esc(km.title)}</span><span class="s-sep">·</span><span class="s-name">${esc(n.label)}</span>` +
      (warns ? `<span class="s-warn" title="warnings">⚠ ${esc(warns)}</span>` : '') +
      `<span class="s-sep">·</span><span class="s-ns">${n.namespace ? esc(n.namespace) : 'cluster'}</span></div>`;
  }).join('');
  if (!results.length) html = '<div class="search-note">no resource matches</div>';
  else if (total > results.length) html += `<div class="search-note">+${esc(total - results.length)} more — refine the search</div>`;
  drop.innerHTML = html;
  placeSearchDrop();
  drop.classList.remove('hidden');
  $('#res-search').setAttribute('aria-expanded', 'true');
  const act = drop.querySelector('.search-row.active');
  if (act) act.scrollIntoView({ block: 'nearest' });
}

// placeSearchDrop pins the dropdown under the input; it is fixed-positioned
// so the sidebar's scroll box does not clip it.
function placeSearchDrop() {
  const r = $('#res-search').getBoundingClientRect();
  const drop = $('#search-drop');
  drop.style.left = r.left + 'px';
  drop.style.top = r.bottom + 4 + 'px';
  drop.style.minWidth = Math.max(300, r.width) + 'px';
}

function chooseSearch(i) {
  const n = state.search.results[i];
  closeSearch();
  if (n) focusNode(n.id);
}

$('#res-search').addEventListener('input', () => refreshSearch(true));
$('#res-search').addEventListener('focus', () => { if ($('#res-search').value.trim()) refreshSearch(true); });
$('#res-search').addEventListener('blur', closeSearch);
$('#res-search').addEventListener('keydown', (ev) => {
  const s = state.search;
  if (ev.key === 'Escape') {
    if (s.open) { ev.stopPropagation(); closeSearch(); }
    return;
  }
  if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') {
    ev.preventDefault();
    if (!s.open) { refreshSearch(true); return; }
    const n = s.results.length;
    if (!n) return;
    s.active = ev.key === 'ArrowDown' ? (s.active + 1) % n : (s.active - 1 + n) % n;
    renderSearchDrop();
  } else if (ev.key === 'Enter') {
    ev.preventDefault();
    if (!s.open) refreshSearch(true);
    if (s.results.length) chooseSearch(Math.max(0, s.active));
  }
});
// mousedown (not click): choosing must happen before the input blurs.
$('#search-drop').addEventListener('mousedown', (ev) => {
  ev.preventDefault();
  const row = ev.target.closest('.search-row');
  if (row) chooseSearch(Number(row.dataset.i));
});
$('#sidebar-overview').addEventListener('scroll', () => { if (state.search.open) placeSearchDrop(); });

// --- D-040: test path field ----------------------------------------------------

// splitHostField moves a pasted "host/path" or URL into both fields (the
// host keeps scheme and port; an explicit, different path field wins).
function splitHostField(hostEl, pathEl) {
  const sp = WTGraph.splitPastedHost(hostEl.value, pathEl.value);
  if (!sp) return;
  hostEl.value = sp.host;
  pathEl.value = sp.path;
}

for (const [h, p] of [['#trace-host', '#trace-path'], ['#ov-host', '#ov-path']]) {
  const hostEl = $(h);
  const pathEl = $(p);
  hostEl.addEventListener('change', () => splitHostField(hostEl, pathEl));
  hostEl.addEventListener('paste', () => setTimeout(() => splitHostField(hostEl, pathEl), 0));
}

// esc HTML-escapes text for element content AND quoted attribute values.
// Entities are generated as numeric references by code ("&#60;" for "<")
// so the source contains no literal entity an editor or tool could decode
// back into the raw character (that is how the old map silently became a
// no-op — qa D-038a F1). scripts/check-ui-escape.mjs guards this.
function esc(s) {
  return String(s ?? '').replace(/[&<>"'`]/g, (ch) => '&#' + ch.charCodeAt(0) + ';');
}

function shorten(s, n) {
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}

window.addEventListener('resize', () => {
  if (state.search.open) placeSearchDrop();
  if (state.cy) state.cy.resize();
  if (state.modalCy) state.modalCy.resize();
  scheduleCardSync();
  scheduleModalCardSync();
});

connect();
refreshHosts(true);
applyFlowStyle();
renderWarnings();
