// watchtower UI — pure graph helpers (no DOM, no cytoscape):
//   * hubLayout: Overview hub-and-spoke layout (D-039) — entry points in the
//     centre, namespace boxes on rings around them, a VS → Service →
//     workload tree inside every box;
//   * chainOf: transitive upstream + downstream traffic chain of a node
//     (B-002 highlight and expand modal);
//   * D-040: tileTone (far-zoom tile priority), collectWarnings (Warnings
//     panel groups), searchNodes (global search ranking), traceQuery /
//     splitPastedHost (host + test path fields).
// Loaded by index.html before app.js (window.WTGraph) and by
// scripts/check-ui-graph.mjs for the unit tests (module.exports).
(function (root) {
  'use strict';

  // Card stacking order inside a box (D-024/D-030/D-037): routing entry
  // points on top, then services, workloads, pods.
  const KIND_ORDER = {
    ingressclass: 0, gateway: 0, ingress: 0, virtualservice: 0.5,
    service: 1, deployment: 2, statefulset: 2, daemonset: 2, pod: 3,
  };
  function kindRank(kind) { return kind in KIND_ORDER ? KIND_ORDER[kind] : 9; }

  // Routing objects start their own column (D-039 tree).
  const ROUTE_KINDS = new Set(['virtualservice', 'ingress', 'gateway', 'httproute', 'grpcroute', 'tlsroute']);

  // Traffic edges carry requests (serves / binds / routes / selects);
  // namespace and config (IngressClass → Ingress) edges do not.
  const TRAFFIC_RE = /^(serves|binds|routes|selects)\b/;
  function isTrafficEdge(e) { return !e.ns && !e.config; }

  const DEFAULTS = {
    gapB: 36,        // between cards along a row (breadth)
    gapD: 44,        // between depth rows (VS → svc → workload)
    bandGap: 60,     // between wrapped bands of columns
    pad: 28,         // compound padding (must match the cytoscape style)
    labelH: 34,      // room for the namespace label above the box
    boxGap: 56,      // between namespace boxes
    emptyW: 160,     // empty namespace (no resources) node size
    emptyH: 44,
    aspect: 1.6,     // target box width / height
    fieldAspect: 1.85, // target width / height of the whole map
    spokeGap: 120,   // between the hub and the inner ring (room for the fan)
    ringGap: 240,    // extra distance of the outer ring beyond the inner one
    southCap: 1.25,  // south row width limit, as a factor of the hub width
    angles: 32,      // candidate directions around the hub
    step: 16,        // push-out step along a direction
  };

  const now = () => (typeof performance !== 'undefined' ? performance.now() : Date.now());
  const byLabel = (a, b) => String(a.label || a.id).localeCompare(String(b.label || b.id));
  const byKind = (a, b) => (kindRank(a.kind) - kindRank(b.kind)) || byLabel(a, b);

  function push(map, k, v) {
    let l = map.get(k);
    if (!l) { l = []; map.set(k, l); }
    l.push(v);
  }

  // chainOf walks traffic edges from start: downstream along edge
  // direction, upstream against it, both transitively. A visited set per
  // direction guards against cycles; at most cap nodes are returned and the
  // rest is counted in truncated ("+N more"). dist is the signed hop
  // distance (upstream < 0 < downstream) for ordering.
  function chainOf(start, edges, opts) {
    const cap = (opts && opts.cap) || 200;
    const out = new Map();
    const inc = new Map();
    for (const e of edges) {
      push(out, e.source, e);
      push(inc, e.target, e);
    }
    const dist = new Map([[start, 0]]);
    const edgeIds = new Set();
    const overflow = new Set();
    const seen = { '1': new Set([start]), '-1': new Set([start]) };
    const queue = [[start, 1], [start, -1]];
    for (let qi = 0; qi < queue.length; qi++) {
      const [id, dir] = queue[qi];
      for (const e of (dir > 0 ? out : inc).get(id) || []) {
        const next = dir > 0 ? e.target : e.source;
        if (seen[dir].has(next)) {
          edgeIds.add(e.id);
          continue;
        }
        if (!dist.has(next) && dist.size >= cap) {
          overflow.add(next);
          continue;
        }
        seen[dir].add(next);
        if (!dist.has(next)) dist.set(next, dist.get(id) + dir);
        edgeIds.add(e.id);
        queue.push([next, dir]);
      }
    }
    return { nodes: new Set(dist.keys()), edges: edgeIds, dist, truncated: overflow.size };
  }

  // --- hub-and-spoke layout (D-039) ---------------------------------------

  // hubLayout computes absolute positions for every leaf node and every
  // empty box. Input:
  //   nodes: [{id, kind, box, w, h, label, ghost}]  (box = compound id)
  //   boxes: [{id, label}]                          (namespace compounds)
  //   edges: [{source, target, config}]             (ns edges left out)
  // Output: {positions: {id: {x, y}}, boxes: {id: {x, y, w, h, orient}},
  //          hubs: [ids], mode: 'hub' | 'grid', ms,
  //          rings: {inner: [box ids], outer: [box ids], hub: rect, innerGap}}.
  function hubLayout(input, options) {
    const t0 = now();
    const o = Object.assign({}, DEFAULTS, options || {});
    const nodes = input.nodes || [];
    const byId = new Map(nodes.map((n) => [n.id, n]));
    const boxList = (input.boxes || []).slice();
    const boxIds = new Set(boxList.map((b) => b.id));
    const members = new Map(boxList.map((b) => [b.id, []]));
    for (const n of nodes) {
      const b = boxIds.has(n.box) ? n.box : '';
      if (!members.has(b)) {
        members.set(b, []);
        boxList.push({ id: b, label: '' });
      }
      members.get(b).push(n);
    }
    const boxOf = (id) => {
      const n = byId.get(id);
      return n ? (boxIds.has(n.box) ? n.box : '') : null;
    };

    const outT = new Map();
    const inT = new Map();
    const linked = new Map(); // any edge (traffic or config), both ways
    for (const e of input.edges || []) {
      if (!byId.has(e.source) || !byId.has(e.target) || e.source === e.target) continue;
      if (!e.config) {
        push(outT, e.source, e.target);
        push(inT, e.target, e.source);
      }
      push(linked, e.source, e.target);
      push(linked, e.target, e.source);
    }

    // Hubs: gateways and real (non-ghost) ingress classes, plus everything
    // upstream of them along traffic edges (gateway workload, LB Service).
    // hubDepth = hops upstream from the entry object.
    const hubDepth = new Map();
    for (const n of nodes) {
      if (n.kind === 'gateway' || (n.kind === 'ingressclass' && !n.ghost)) hubDepth.set(n.id, 0);
    }
    const hq = [...hubDepth.keys()];
    for (let i = 0; i < hq.length; i++) {
      const d = hubDepth.get(hq[i]);
      if (d >= 4) continue;
      for (const s of inT.get(hq[i]) || []) {
        if (!hubDepth.has(s)) {
          hubDepth.set(s, d + 1);
          hq.push(s);
        }
      }
    }

    const layouts = new Map(); // box id → {v: vertical, h: horizontal}
    const boxLayoutFor = (id, horiz) => {
      let l = layouts.get(id);
      if (!l) { l = {}; layouts.set(id, l); }
      const k = horiz ? 'h' : 'v';
      if (!l[k]) l[k] = layoutBox(members.get(id) || [], horiz, o, { byId, outT, inT, hubDepth });
      return l[k];
    };
    const dims = (id, orient) => {
      const l = boxLayoutFor(id, orient === 'left' || orient === 'right');
      const pad = l.empty ? 0 : o.pad;
      return { w: l.W + 2 * pad, h: l.H + 2 * pad + o.labelH, l };
    };

    const positions = {};
    const boxesOut = {};
    const place = (id, cx, cy, orient) => {
      const d = dims(id, orient);
      const left = cx - d.w / 2;
      const top = cy - d.h / 2;
      boxesOut[id] = { x: left, y: top, w: d.w, h: d.h, orient };
      if (d.l.empty) {
        positions[id] = { x: cx, y: top + o.labelH + o.emptyH / 2 };
        return boxesOut[id];
      }
      for (const [nid, p] of d.l.local(orient)) {
        positions[nid] = { x: left + o.pad + p.x, y: top + o.labelH + o.pad + p.y };
      }
      return boxesOut[id];
    };

    const rings = { inner: [], outer: [], hub: null, innerGap: 0 };
    const hubBoxes = boxList.filter((b) => (members.get(b.id) || []).some((n) => hubDepth.has(n.id)));
    let mode = 'hub';
    if (!hubBoxes.length) {
      mode = 'grid';
      gridPlace(boxList, dims, place, o);
    } else {
      // Centre: hub boxes side by side in traffic order (LB Service box
      // first, gateway box after it).
      const hubRank = (b) => Math.max(...members.get(b.id).filter((n) => hubDepth.has(n.id)).map((n) => hubDepth.get(n.id)));
      hubBoxes.sort((a, b) => (hubRank(b) - hubRank(a)) || byLabel(a, b));
      const hd = hubBoxes.map((b) => dims(b.id, 'down'));
      let x = -(hd.reduce((s, d) => s + d.w, 0) + o.boxGap * (hd.length - 1)) / 2;
      const placed = [];
      hubBoxes.forEach((b, i) => {
        placed.push(place(b.id, x + hd[i].w / 2, 0, 'down'));
        x += hd[i].w + o.boxGap;
      });
      const hubU = union(placed);
      rings.hub = hubU;
      const hc = { x: hubU.x + hubU.w / 2, y: hubU.y + hubU.h / 2 };
      const hubSet = new Set(hubBoxes.map((b) => b.id));

      // Inner ring: boxes holding nodes linked to a hub node (VS bound to a
      // gateway, Ingress of a real class). Attachment = the hub nodes they
      // link to; most-connected boxes first.
      const rest = boxList.filter((b) => !hubSet.has(b.id));
      const attach = new Map();
      for (const b of rest) {
        const pts = [];
        for (const n of members.get(b.id) || []) {
          for (const other of linked.get(n.id) || []) {
            if (hubDepth.has(other) && boxOf(other) !== b.id && positions[other]) pts.push(positions[other]);
          }
        }
        if (pts.length) attach.set(b.id, pts);
      }
      const area = (b) => { const d = dims(b.id, 'down'); return d.w * d.h; };
      const inner = rest.filter((b) => attach.has(b.id))
        .sort((a, b) => (attach.get(b.id).length - attach.get(a.id).length) || (area(b) - area(a)) || byLabel(a, b));
      const outer = rest.filter((b) => !attach.has(b.id)).sort((a, b) => (area(b) - area(a)) || byLabel(a, b));
      rings.inner = inner.map((b) => b.id);
      rings.outer = outer.map((b) => b.id);

      placeSouthFirst(inner, attach, { hubU, hc, placed, dims, place, o });

      // Outer ring: strictly farther from the hub than every inner box, on
      // the sides the inner ring left free or outside it.
      for (const b of inner) rings.innerGap = Math.max(rings.innerGap, rectGap(boxesOut[b.id], hubU));
      const zone = union(placed);
      const zc = { x: zone.x + zone.w / 2, y: zone.y + zone.h / 2 };
      const clear = rings.innerGap + o.ringGap;
      const obstacles = placed.concat([zone, {
        x: hubU.x - clear, y: hubU.y - clear, w: hubU.w + 2 * clear, h: hubU.h + 2 * clear,
      }]);
      for (const b of outer) {
        const best = bestSpot(b.id, zc, obstacles, dims, o,
          (c) => Math.hypot((c.x - zc.x) / o.fieldAspect, c.y - zc.y));
        obstacles.push(place(b.id, best.c.x, best.c.y, 'down'));
      }
    }
    return { positions, boxes: boxesOut, hubs: [...hubDepth.keys()], mode, rings, ms: now() - t0 };
  }

  // placeSouthFirst fills the inner ring: south of the hub first (orient
  // down: VS on top facing the hub, then Service, then workload), then the
  // east and west sides (orient right/left), and the north arc (orient up)
  // only when those are exhausted.
  function placeSouthFirst(inner, attach, ctx) {
    const { hubU, hc, placed, dims, place, o } = ctx;
    let i = 0;
    // South row: at most southCap x hub width (the two widest boxes always
    // fit), most-connected box in the middle, the others alternating to its
    // sides.
    const south = [];
    const widths = inner.map((b) => dims(b.id, 'down').w).sort((a, b) => b - a);
    const cap = Math.max(hubU.w * o.southCap, (widths[0] || 0) + o.boxGap + (widths[1] || 0));
    let used = 0;
    while (i < inner.length) {
      const d = dims(inner[i].id, 'down');
      if (south.length && used + o.boxGap + d.w > cap) break;
      used += (south.length ? o.boxGap : 0) + d.w;
      if (south.length % 2) south.push(inner[i]); else south.unshift(inner[i]);
      i++;
    }
    if (south.length) {
      const top = hubU.y + hubU.h + o.spokeGap;
      // Shift the row so its attachment-weighted middle sits under the hub
      // nodes it links to (short spokes).
      let cx = 0;
      const items = south.map((b) => {
        const d = dims(b.id, 'down');
        const it = { b, d, x: cx + d.w / 2 };
        cx += d.w + o.boxGap;
        return it;
      });
      let sum = 0;
      let wx = 0;
      let ax = 0;
      for (const it of items) {
        const pts = attach.get(it.b.id);
        sum += pts.length;
        wx += it.x * pts.length;
        for (const p of pts) ax += p.x;
      }
      const shift = ax / sum - wx / sum;
      for (const it of items) placed.push(place(it.b.id, it.x + shift, top + it.d.h / 2, 'down'));
    }
    // East / west stacks within the hub's vertical span.
    let eastY = hubU.y;
    let westY = hubU.y;
    let eastN = 0;
    let westN = 0;
    const north = [];
    const fits = (y, n, d) => n === 0 || (y + d.h <= hubU.y + hubU.h);
    for (; i < inner.length; i++) {
      const b = inner[i];
      const dE = dims(b.id, 'right');
      const dW = dims(b.id, 'left');
      const preferEast = eastY <= westY;
      const east = preferEast ? fits(eastY, eastN, dE) : !fits(westY, westN, dW);
      const d = east ? dE : dW;
      if (!(east ? fits(eastY, eastN, dE) : fits(westY, westN, dW))) { north.push(b); continue; }
      const sign = east ? 1 : -1;
      const y = east ? eastY : westY;
      let fx = (east ? hubU.x + hubU.w : hubU.x) + sign * (o.spokeGap + d.w / 2);
      // A wide south row can reach under a tall side box: slide out.
      const clashes = (cx) => placed.some((ob) => overlaps({ x: cx - d.w / 2, y, w: d.w, h: d.h }, ob, o.boxGap));
      while (clashes(fx)) fx += sign * o.step;
      placed.push(place(b.id, fx, y + d.h / 2, east ? 'right' : 'left'));
      if (east) { eastY = y + d.h + o.boxGap; eastN++; } else { westY = y + d.h + o.boxGap; westN++; }
    }
    // North rows, growing upwards.
    let bottom = hubU.y - o.spokeGap;
    for (let n = 0; n < north.length;) {
      const row = [];
      let w = 0;
      while (n < north.length) {
        const d = dims(north[n].id, 'up');
        if (row.length && w + o.boxGap + d.w > cap) break;
        w += (row.length ? o.boxGap : 0) + d.w;
        row.push({ b: north[n], d });
        n++;
      }
      let x = hc.x - w / 2;
      let rowH = 0;
      for (const it of row) {
        placed.push(place(it.b.id, x + it.d.w / 2, bottom - it.d.h / 2, 'up'));
        x += it.d.w + o.boxGap;
        rowH = Math.max(rowH, it.d.h);
      }
      bottom -= rowH + o.boxGap;
    }
  }

  // bestSpot tries every direction around the map centre, pushes the box
  // out until it collides with nothing, and keeps the cheapest spot.
  function bestSpot(id, centre, obstacles, dims, o, cost) {
    let best = null;
    const d = dims(id, 'down');
    for (let k = 0; k < o.angles; k++) {
      const theta = -Math.PI / 2 + (k * 2 * Math.PI) / o.angles;
      const dx = Math.cos(theta);
      const dy = Math.sin(theta);
      for (let r = 0; r < 200000; r += o.step) {
        const c = { x: centre.x + r * dx, y: centre.y + r * dy };
        const rect = { x: c.x - d.w / 2, y: c.y - d.h / 2, w: d.w, h: d.h };
        if (!obstacles.some((ob) => overlaps(rect, ob, o.boxGap))) {
          const v = cost(c);
          if (!best || v < best.v - 0.5) best = { v, c };
          break;
        }
      }
    }
    return best;
  }

  // gridPlace is the fallback without any hub (D-022 grid of boxes, now
  // compound boxes with the tree layout inside).
  function gridPlace(boxList, dims, place, o) {
    const list = boxList.slice().sort((a, b) => {
      const ca = a.id === 'cell:cluster' ? 0 : 1;
      const cb = b.id === 'cell:cluster' ? 0 : 1;
      return (ca - cb) || byLabel(a, b);
    });
    const cols = Math.max(1, Math.ceil(Math.sqrt(list.length)));
    const colW = new Array(cols).fill(0);
    const rowH = [];
    const d = list.map((b) => dims(b.id, 'down'));
    list.forEach((b, i) => {
      colW[i % cols] = Math.max(colW[i % cols], d[i].w);
      const r = Math.floor(i / cols);
      rowH[r] = Math.max(rowH[r] || 0, d[i].h);
    });
    const colX = [];
    let x = 0;
    for (let c = 0; c < cols; c++) { colX[c] = x; x += colW[c] + o.boxGap; }
    const rowY = [];
    let y = 0;
    for (let r = 0; r < rowH.length; r++) { rowY[r] = y; y += rowH[r] + o.boxGap; }
    list.forEach((b, i) => {
      const c = i % cols;
      const r = Math.floor(i / cols);
      place(b.id, colX[c] + d[i].w / 2, rowY[r] + d[i].h / 2, 'down');
    });
  }

  // layoutBox lays out one namespace box in a local (u = breadth, v =
  // depth) frame: every route (and hub chain, and Service → workload chain)
  // starts a column; children sit one depth row below their parent; a node
  // shared by two parents stays under the first. Isolated resources fill
  // compact columns at the end. Columns wrap into bands to keep the box
  // near the target aspect. horiz = depth runs along x (left/right boxes).
  function layoutBox(list, horiz, o, g) {
    if (!list.length) return { W: o.emptyW, H: o.emptyH, empty: true, local: () => new Map() };
    const inBox = new Set(list.map((n) => n.id));
    const bsz = (n) => (horiz ? n.h : n.w);
    const dsz = (n) => (horiz ? n.w : n.h);
    const kidsOf = (id) => (g.outT.get(id) || []).filter((t) => inBox.has(t)).map((t) => g.byId.get(t)).sort(byKind);
    const hasInParent = (id) => (g.inT.get(id) || []).some((s) => inBox.has(s) && s !== id);
    const connected = (id) => (g.outT.get(id) || []).length > 0 || (g.inT.get(id) || []).length > 0;

    const hubs = list.filter((n) => g.hubDepth.has(n.id))
      .sort((a, b) => (g.hubDepth.get(b.id) - g.hubDepth.get(a.id)) || byLabel(a, b));
    const routes = list.filter((n) => !g.hubDepth.has(n.id) && ROUTE_KINDS.has(n.kind)).sort(byLabel);
    const chainRoots = list.filter((n) => !g.hubDepth.has(n.id) && !ROUTE_KINDS.has(n.kind) &&
      !hasInParent(n.id) && kidsOf(n.id).length > 0).sort(byKind);
    const others = list.filter((n) => connected(n.id)).sort(byKind);
    const iso = list.filter((n) => !connected(n.id)).sort(byKind);

    const claimed = new Set();
    const grow = (n, depth) => {
      claimed.add(n.id);
      const t = { n, depth, kids: [] };
      for (const k of kidsOf(n.id)) {
        if (!claimed.has(k.id)) t.kids.push(grow(k, depth + 1));
      }
      return t;
    };
    const trees = [];
    for (const n of hubs.concat(routes, chainRoots, others)) {
      if (!claimed.has(n.id)) trees.push(grow(n, 0));
    }

    // Columns: {span, depth, items: [{n, u, d}]} with u relative to the
    // column start.
    const columns = [];
    let maxDepth = 0;
    const measure = (t) => {
      let s = 0;
      for (const k of t.kids) s += measure(k);
      if (t.kids.length) s += o.gapB * (t.kids.length - 1);
      t.span = Math.max(bsz(t.n), s);
      return t.span;
    };
    const assign = (t, u0, items) => {
      items.push({ n: t.n, u: u0 + t.span / 2, d: t.depth });
      maxDepth = Math.max(maxDepth, t.depth + 1);
      let kidsSpan = 0;
      for (const k of t.kids) kidsSpan += k.span;
      kidsSpan += o.gapB * Math.max(0, t.kids.length - 1);
      let cu = u0 + (t.span - kidsSpan) / 2;
      for (const k of t.kids) {
        assign(k, cu, items);
        cu += k.span + o.gapB;
      }
    };
    for (const t of trees) {
      measure(t);
      const items = [];
      const before = maxDepth;
      maxDepth = 0;
      assign(t, 0, items);
      columns.push({ span: t.span, depth: maxDepth, items, hub: g.hubDepth.has(t.n.id) });
      maxDepth = Math.max(before, maxDepth);
    }
    if (iso.length) {
      const avgB = iso.reduce((s, n) => s + bsz(n), 0) / iso.length;
      const avgD = iso.reduce((s, n) => s + dsz(n), 0) / iso.length;
      const ratio = horiz ? 1 / o.aspect : o.aspect;
      const rows = trees.length
        ? Math.max(2, maxDepth)
        : Math.max(1, Math.round(Math.sqrt((iso.length * (avgB + o.gapB)) / (ratio * (avgD + o.gapD)))));
      for (let i = 0; i < iso.length; i += rows) {
        const chunk = iso.slice(i, i + rows);
        const span = Math.max(...chunk.map(bsz));
        columns.push({ span, depth: chunk.length, items: chunk.map((n, j) => ({ n, u: span / 2, d: j })) });
      }
    }

    // Hub chains close the box: they end up on its south edge, facing the
    // inner ring, so their spokes do not cross the box's other cards.
    columns.sort((a, b) => Number(!!a.hub) - Number(!!b.hub));

    // Wrap columns into bands near the target aspect.
    const rowAll = [];
    for (const c of columns) {
      for (const it of c.items) rowAll[it.d] = Math.max(rowAll[it.d] || 0, dsz(it.n));
    }
    const colExtent = (c) => {
      let v = 0;
      for (let d = 0; d < c.depth; d++) v += (rowAll[d] || 0) + (d ? o.gapD : 0);
      return v;
    };
    let area = 0;
    let maxSpan = 0;
    for (const c of columns) {
      area += (c.span + o.gapB) * (colExtent(c) + o.bandGap);
      maxSpan = Math.max(maxSpan, c.span);
    }
    const ratio = horiz ? 1 / o.aspect : o.aspect;
    const target = Math.max(maxSpan, Math.sqrt(area * ratio));
    const bands = [];
    let cur = null;
    for (const c of columns) {
      const hubBreak = c.hub && cur && cur.cols.length && !cur.cols[cur.cols.length - 1].hub;
      if (!cur || hubBreak || (cur.width > 0 && cur.width + o.gapB + c.span > target)) {
        cur = { cols: [], width: 0 };
        bands.push(cur);
      }
      c.start = cur.width ? cur.width + o.gapB : 0;
      cur.width = c.start + c.span;
      cur.cols.push(c);
    }
    const local = new Map(); // id → {u, v} before orientation
    let U = 0;
    let V = 0;
    for (const b of bands) {
      const rows = [];
      for (const c of b.cols) {
        for (const it of c.items) rows[it.d] = Math.max(rows[it.d] || 0, dsz(it.n));
      }
      const rowStart = [];
      let v = 0;
      for (let d = 0; d < rows.length; d++) {
        rowStart[d] = v;
        v += (rows[d] || 0) + o.gapD;
      }
      const bandDepth = v - o.gapD;
      for (const c of b.cols) {
        for (const it of c.items) {
          local.set(it.n.id, { u: c.start + it.u, v: V + rowStart[it.d] + (rows[it.d] || 0) / 2 });
        }
      }
      U = Math.max(U, b.width);
      V += bandDepth + o.bandGap;
    }
    V -= o.bandGap;
    return {
      W: horiz ? V : U,
      H: horiz ? U : V,
      empty: false,
      local(orient) {
        const out = new Map();
        for (const [id, p] of local) {
          switch (orient) {
            case 'up': out.set(id, { x: p.u, y: V - p.v }); break;
            case 'right': out.set(id, { x: p.v, y: p.u }); break;
            case 'left': out.set(id, { x: V - p.v, y: p.u }); break;
            default: out.set(id, { x: p.u, y: p.v });
          }
        }
        return out;
      },
    };
  }

  // rectGap is the shortest distance between two rectangles (0 when they
  // touch or overlap).
  function rectGap(a, b) {
    const dx = Math.max(0, Math.max(a.x - (b.x + b.w), b.x - (a.x + a.w)));
    const dy = Math.max(0, Math.max(a.y - (b.y + b.h), b.y - (a.y + a.h)));
    return Math.hypot(dx, dy);
  }

  // keepResolved drops edges with a missing endpoint: cytoscape throws on
  // those and one bad edge would abort the whole render.
  function keepResolved(edges, ids) {
    return edges.filter((e) => ids.has(e.from) && ids.has(e.to));
  }

  function overlaps(a, b, gap) {
    return a.x < b.x + b.w + gap && b.x < a.x + a.w + gap &&
      a.y < b.y + b.h + gap && b.y < a.y + a.h + gap;
  }

  function union(rects) {
    let x0 = Infinity;
    let y0 = Infinity;
    let x1 = -Infinity;
    let y1 = -Infinity;
    for (const r of rects) {
      x0 = Math.min(x0, r.x);
      y0 = Math.min(y0, r.y);
      x1 = Math.max(x1, r.x + r.w);
      y1 = Math.max(y1, r.y + r.h);
    }
    return { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
  }

  // --- D-040: warning visibility, global search, test path ----------------

  // tileTone is the far-zoom tile colour class of a node, in priority
  // order: a bad node (ok=false) is "fail", a node with at least one
  // warning is "warn", routing objects keep "routing", the rest "".
  function tileTone(n) {
    if (!n) return '';
    if (n.ok === false) return 'fail';
    if ((n.warnings || []).length) return 'warn';
    if (ROUTING_TONE_KINDS.has(n.kind)) return 'routing';
    return '';
  }
  const ROUTING_TONE_KINDS = new Set(['gateway', 'virtualservice', 'ingressclass']);

  // collectWarnings flattens every node warning (and every ok=false edge,
  // as a "broken link" on its source node) of a topology into groups:
  // namespace → resource, both sorted by name (cluster-scoped resources
  // under ""), rows keyed by node id + reason (+ #n for repeats) so UI open
  // state never depends on message text. Optional filter: { text, ns }.
  function collectWarnings(graph, filter) {
    const f = filter || {};
    const text = String(f.text || '').trim().toLowerCase();
    const byId = new Map();
    for (const n of (graph && graph.nodes) || []) byId.set(n.id, n);
    const resources = new Map(); // node id → resource entry
    let total = 0;
    const entry = (n) => {
      let r = resources.get(n.id);
      if (!r) {
        r = { id: n.id, kind: n.kind || '', label: n.label || n.id, namespace: n.namespace || '', rows: [], seen: new Map() };
        resources.set(n.id, r);
      }
      return r;
    };
    const addRow = (r, row) => {
      const base = r.id + '|' + row.reason;
      const k = (r.seen.get(base) || 0) + 1;
      r.seen.set(base, k);
      row.key = k > 1 ? base + '#' + k : base;
      r.rows.push(row);
      total++;
    };
    for (const n of (graph && graph.nodes) || []) {
      for (const w of n.warnings || []) {
        addRow(entry(n), { type: 'warning', reason: w.reason || 'warning', message: w.message || w.reason || '', hint: w.hint || '', items: w.items || [] });
      }
    }
    for (const e of (graph && graph.edges) || []) {
      if (e.ok !== false) continue;
      const from = byId.get(e.from);
      const to = byId.get(e.to);
      if (!from) continue;
      const target = to ? (to.namespace && to.namespace !== from.namespace ? to.namespace + '/' : '') + (to.label || to.id) : e.to;
      addRow(entry(from), {
        type: 'broken',
        reason: 'brokenLink→' + e.to,
        message: 'broken link → ' + target + (e.label ? ' (' + e.label + ')' : ''),
        hint: '',
        items: [],
        target: e.to,
      });
    }
    const groups = new Map();
    let shown = 0;
    for (const r of resources.values()) {
      delete r.seen;
      if (f.ns !== undefined && f.ns !== '' && r.namespace !== f.ns) continue;
      if (text) {
        const head = (r.kind + ' ' + r.namespace + '/' + r.label).toLowerCase();
        r.rows = r.rows.filter((row) => head.includes(text) ||
          (row.message + ' ' + row.hint + ' ' + row.reason + ' ' + row.items.join(' ')).toLowerCase().includes(text));
        if (!r.rows.length) continue;
      }
      shown += r.rows.length;
      push(groups, r.namespace, r);
    }
    const out = [...groups.entries()]
      .sort((a, b) => a[0].localeCompare(b[0]))
      .map(([ns, list]) => ({
        namespace: ns,
        resources: list.sort((a, b) => a.label.localeCompare(b.label) || a.kind.localeCompare(b.kind) || a.id.localeCompare(b.id)),
      }));
    return { groups: out, total, shown };
  }

  // Kind words accepted by the global search (D-040): full kind names
  // match by prefix (>= 2 chars), short aliases exactly.
  const KIND_ALIASES = {
    deploy: 'deployment', dep: 'deployment', sts: 'statefulset', ds: 'daemonset',
    svc: 'service', ing: 'ingress', vs: 'virtualservice', gw: 'gateway',
    ingc: 'ingressclass', ns: 'namespace', po: 'pod',
  };
  function kindMatches(token, kind) {
    if (!kind) return false;
    if (KIND_ALIASES[token] === kind) return true;
    return token.length >= 2 && kind.startsWith(token);
  }

  // nameScore ranks how a token matches a name: exact 0, prefix 1, start
  // of a word (after - . _ /) 2, substring 3, none -1.
  function nameScore(token, name) {
    if (name === token) return 0;
    if (name.startsWith(token)) return 1;
    const i = name.indexOf(token);
    if (i < 0) return -1;
    return /[-._/]/.test(name[i - 1]) ? 2 : 3;
  }

  // searchNodes is the global resource search: case-insensitive, every
  // whitespace-separated token must match the name, "namespace/name" or a
  // kind word. Ranked by the best name match (exact, prefix, word start,
  // substring, kind/namespace only), then kind order, name, namespace.
  // Returns at most limit results plus the total match count.
  function searchNodes(nodes, query, limit) {
    const tokens = String(query || '').toLowerCase().trim().split(/\s+/).filter(Boolean);
    const max = limit || 12;
    if (!tokens.length) return { results: [], total: 0 };
    const hits = [];
    for (const n of nodes || []) {
      const name = String(n.label || '').toLowerCase();
      const ns = String(n.namespace || '').toLowerCase();
      const full = ns + '/' + name;
      const kind = String(n.kind || '').toLowerCase();
      let best = 9;
      let all = true;
      for (const t of tokens) {
        const sc = nameScore(t, name);
        if (sc >= 0) { best = Math.min(best, sc); continue; }
        if (t.includes('/') && full.includes(t)) { best = Math.min(best, 4); continue; }
        if (kindMatches(t, kind)) { best = Math.min(best, 5); continue; }
        all = false;
        break;
      }
      if (all) hits.push({ node: n, score: best });
    }
    hits.sort((a, b) => (a.score - b.score) ||
      (kindRank(a.node.kind) - kindRank(b.node.kind)) ||
      String(a.node.label || '').localeCompare(String(b.node.label || '')) ||
      String(a.node.namespace || '').localeCompare(String(b.node.namespace || '')) ||
      String(a.node.id).localeCompare(String(b.node.id)));
    return { results: hits.slice(0, max).map((h) => h.node), total: hits.length };
  }

  // parseHostInput splits "[scheme://]host[:port][/path]" (D-038a).
  function parseHostInput(raw) {
    let v = String(raw || '').trim();
    let scheme = '';
    const m = /^(https?):\/\//i.exec(v);
    if (m) { scheme = m[1].toLowerCase(); v = v.slice(m[0].length); }
    let path = '';
    const i = v.search(/[/?#]/);
    if (i >= 0) {
      if (v[i] === '/') path = v.slice(i);
      v = v.slice(0, i);
    }
    return { host: v, path, scheme };
  }

  // normPath gives a path field value a leading slash ("" stays "").
  function normPath(p) {
    const v = String(p || '').trim();
    if (!v) return '';
    return v.startsWith('/') ? v : '/' + v;
  }

  // traceQuery merges the host input and the optional test path field
  // (D-040): a path pasted into the host is used unless the path field is
  // set too and differs — then the field wins. No path at all means "/".
  function traceQuery(rawHost, rawPath) {
    const p = parseHostInput(rawHost);
    const field = normPath(rawPath);
    return { host: p.host, scheme: p.scheme, path: field || p.path || '/' };
  }

  // splitPastedHost is the "fill both fields" step of a pasted host/path
  // or URL: the host keeps scheme and host[:port], the path moves to the
  // path field unless that field already holds a different path (field
  // wins). Returns null when there is nothing to split.
  function splitPastedHost(rawHost, rawPath) {
    const p = parseHostInput(rawHost);
    const raw = String(rawHost || '').trim();
    const hostOnly = (p.scheme ? p.scheme + '://' : '') + p.host;
    if (hostOnly === raw) return null;
    return { host: hostOnly, path: normPath(rawPath) || p.path };
  }

  const api = {
    chainOf, hubLayout, isTrafficEdge, keepResolved, kindRank, overlaps, rectGap, TRAFFIC_RE, KIND_ORDER, DEFAULTS,
    tileTone, collectWarnings, searchNodes, parseHostInput, traceQuery, splitPastedHost,
  };
  root.WTGraph = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof globalThis !== 'undefined' ? globalThis : this);
