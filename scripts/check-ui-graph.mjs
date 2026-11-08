#!/usr/bin/env node
// Unit checks for the pure Overview graph helpers (ui/graphkit.js, D-039 /
// B-002): chainOf cycle safety and capping, hubLayout overlap-freeness and
// fallback. Usage: node scripts/check-ui-graph.mjs [path/to/graphkit.js]
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';

const require = createRequire(import.meta.url);
const file = resolve(process.argv[2] || join(dirname(fileURLToPath(import.meta.url)), '..', 'ui', 'graphkit.js'));
const G = require(file);

let failed = 0;
function ok(cond, msg) {
  if (!cond) {
    failed++;
    console.error('FAIL: ' + msg);
  }
}

// --- chainOf ---------------------------------------------------------------
const e = (id, source, target) => ({ id, source, target });

{
  // LB → dep → gw → vs → svc → dep → pod; a sibling route must not join.
  const edges = [
    e('1', 'lb', 'igw'), e('2', 'igw', 'gw'), e('3', 'gw', 'vs'), e('4', 'gw', 'vs2'),
    e('5', 'vs', 'svc'), e('6', 'svc', 'dep'), e('7', 'dep', 'pod'), e('8', 'vs2', 'svc2'),
  ];
  const c = G.chainOf('vs', edges);
  const want = ['lb', 'igw', 'gw', 'vs', 'svc', 'dep', 'pod'];
  ok(c.nodes.size === want.length && want.every((n) => c.nodes.has(n)), `chainOf vs: ${[...c.nodes]}`);
  ok(!c.nodes.has('vs2') && !c.nodes.has('svc2'), 'sibling route must not be part of the chain');
  ok(c.edges.has('2') && c.edges.has('7') && !c.edges.has('4'), `chain edges: ${[...c.edges]}`);
  ok(c.dist.get('lb') < 0 && c.dist.get('pod') > 0 && c.dist.get('vs') === 0, 'dist is signed upstream/downstream');
  ok(c.truncated === 0, 'no truncation');
}
{
  // Cycles terminate and keep every member once.
  const c = G.chainOf('a', [e('1', 'a', 'b'), e('2', 'b', 'c'), e('3', 'c', 'a'), e('4', 'b', 'a')]);
  ok(c.nodes.size === 3, `cycle nodes: ${[...c.nodes]}`);
}
{
  // Cap with overflow count.
  const edges = [];
  for (let i = 0; i < 300; i++) edges.push(e(`e${i}`, 'root', `n${i}`));
  const c = G.chainOf('root', edges, { cap: 200 });
  ok(c.nodes.size === 200 && c.truncated === 101, `cap: ${c.nodes.size} + ${c.truncated}`);
}
{
  ok(G.isTrafficEdge({}), 'plain edge is traffic');
  ok(!G.isTrafficEdge({ ns: true }) && !G.isTrafficEdge({ config: true }), 'ns/config edges are not traffic');
  ok(G.TRAFFIC_RE.test('routes → 80') && G.TRAFFIC_RE.test('selects') && !G.TRAFFIC_RE.test('class'), 'TRAFFIC_RE');
}

{
  // Edges with a missing endpoint are dropped before cy.add.
  const ids = new Set(['a', 'b']);
  const kept = G.keepResolved([{ from: 'a', to: 'b' }, { from: 'a', to: 'x' }, { from: 'y', to: 'b' }], ids);
  ok(kept.length === 1 && kept[0].to === 'b', `keepResolved: ${JSON.stringify(kept)}`);
}

// --- hubLayout ---------------------------------------------------------------
function fixture({ hub = true, pad = 12 } = {}) {
  const nodes = [];
  const boxes = [];
  const edges = [];
  const add = (id, kind, box, h = 120) => { nodes.push({ id, kind, box, w: 240, h, label: id }); return id; };
  for (const name of ['istio-system', 'default', 'production', 'util', 'monitoring', 'argocd', 'dev', 'empty']) {
    boxes.push({ id: 'ns:' + name, label: name });
  }
  if (hub) {
    add('lb', 'service', 'ns:istio-system', 300);
    add('igw', 'deployment', 'ns:istio-system', 200);
    add('gw', 'gateway', 'ns:default');
    edges.push({ source: 'lb', target: 'igw' }, { source: 'igw', target: 'gw' });
  }
  for (const ns of ['production', 'util']) {
    for (let i = 0; i < pad; i++) {
      const vs = add(`vs:${ns}/${i}`, 'virtualservice', 'ns:' + ns);
      const svc = add(`svc:${ns}/${i}`, 'service', 'ns:' + ns, 90 + (i % 3) * 40);
      const dep = add(`dep:${ns}/${i}`, 'deployment', 'ns:' + ns);
      if (hub) edges.push({ source: 'gw', target: vs });
      edges.push({ source: vs, target: svc }, { source: svc, target: dep });
    }
  }
  for (const ns of ['monitoring', 'argocd']) {
    for (let i = 0; i < 18; i++) add(`p:${ns}/${i}`, i % 2 ? 'service' : 'deployment', 'ns:' + ns, 80 + (i % 5) * 30);
    for (let i = 0; i + 1 < 18; i += 2) edges.push({ source: `p:${ns}/${i + 1}`, target: `p:${ns}/${i}` });
  }
  add('standalone', 'pod', 'ns:dev');
  // a dependency cycle and a shared Service must not break the tree
  add('x1', 'service', 'ns:dev'); add('x2', 'deployment', 'ns:dev');
  edges.push({ source: 'x1', target: 'x2' }, { source: 'x2', target: 'x1' });
  edges.push({ source: 'vs:util/0', target: 'svc:util/1' });
  return { nodes, boxes, edges };
}

function checkLayout(name, input, res) {
  const rects = input.nodes.map((n) => {
    const p = res.positions[n.id];
    ok(!!p, `${name}: ${n.id} has a position`);
    return p ? { id: n.id, box: n.box, x: p.x - n.w / 2, y: p.y - n.h / 2, w: n.w, h: n.h } : null;
  }).filter(Boolean);
  for (let i = 0; i < rects.length; i++) {
    for (let j = i + 1; j < rects.length; j++) {
      ok(!G.overlaps(rects[i], rects[j], 0), `${name}: cards overlap ${rects[i].id} / ${rects[j].id}`);
    }
  }
  const boxes = Object.entries(res.boxes);
  for (let i = 0; i < boxes.length; i++) {
    for (let j = i + 1; j < boxes.length; j++) {
      ok(!G.overlaps(boxes[i][1], boxes[j][1], 0), `${name}: boxes overlap ${boxes[i][0]} / ${boxes[j][0]}`);
    }
  }
  for (const r of rects) {
    const b = res.boxes[r.box];
    ok(b && r.x >= b.x && r.y >= b.y && r.x + r.w <= b.x + b.w && r.y + r.h <= b.y + b.h, `${name}: ${r.id} outside its box`);
  }
}

{
  const input = fixture({ hub: true });
  const res = G.hubLayout(input);
  ok(res.mode === 'hub', `mode: ${res.mode}`);
  ok(['gw', 'igw', 'lb'].every((h) => res.hubs.includes(h)), `hubs: ${res.hubs}`);
  checkLayout('hub', input, res);
  // Hub boxes sit around the centre, connected boxes on the inner ring.
  const d = (id) => {
    const b = res.boxes[id];
    return Math.hypot(b.x + b.w / 2, b.y + b.h / 2);
  };
  ok(d('ns:default') < d('ns:monitoring') && d('ns:istio-system') < d('ns:monitoring'), 'hub boxes are closer to the centre than the outer ring');
  ok(d('ns:production') < d('ns:argocd') && d('ns:util') < d('ns:argocd'), 'routed namespaces are on the inner ring');
  // VS columns: the route sits on the box side facing the hub.
  const prod = res.boxes['ns:production'];
  const vs = res.positions['vs:production/0'];
  const svc = res.positions['svc:production/0'];
  const c = { x: prod.x + prod.w / 2, y: prod.y + prod.h / 2 };
  const dirToHub = Math.hypot(0 - c.x, 0 - c.y) || 1;
  const toward = (p) => ((p.x - c.x) * (0 - c.x) + (p.y - c.y) * (0 - c.y)) / dirToHub;
  ok(toward(vs) > toward(svc), 'VS sits closer to the hub side than its Service');
}
{
  // F2/F3: routed boxes south of the hub, orient=down (VS on top, Service,
  // then workload); the outer ring clearly beyond the inner one.
  const input = fixture({ hub: true });
  const res = G.hubLayout(input);
  const hub = res.rings.hub;
  for (const id of ['ns:production', 'ns:util']) {
    const b = res.boxes[id];
    ok(b.orient === 'down' && b.y >= hub.y + hub.h, `${id}: south with orient=down (${b.orient}, y=${b.y})`);
  }
  for (const ns of ['production', 'util']) {
    const vs = res.positions[`vs:${ns}/0`];
    const svc = res.positions[`svc:${ns}/0`];
    const dep = res.positions[`dep:${ns}/0`];
    ok(vs.y < svc.y && svc.y < dep.y, `${ns}: VS above Service above workload`);
  }
  ok(res.rings.inner.length === 2 && res.rings.outer.length > 0, `rings: ${JSON.stringify(res.rings.outer)}`);
  const minOuter = Math.min(...res.rings.outer.map((id) => G.rectGap(res.boxes[id], hub)));
  ok(minOuter > res.rings.innerGap, `outer gap ${minOuter} must exceed inner gap ${res.rings.innerGap}`);
}
{
  // Many routed boxes: south first, then east/west, north only as overflow.
  const nodes = [{ id: 'gw', kind: 'gateway', box: 'ns:hub', w: 240, h: 100, label: 'gw' }];
  const boxes = [{ id: 'ns:hub', label: 'hub' }];
  const edges = [];
  const N = 14;
  for (let i = 0; i < N; i++) {
    boxes.push({ id: `ns:r${i}`, label: `r${i}` });
    nodes.push({ id: `vs${i}`, kind: 'virtualservice', box: `ns:r${i}`, w: 240, h: 100, label: `vs${i}` });
    edges.push({ source: 'gw', target: `vs${i}` });
  }
  // the first routed box keeps the hub tree busy: hub is tiny, cap is one box
  const res = G.hubLayout({ nodes, boxes, edges });
  checkLayout('overflow', { nodes, boxes, edges }, res);
  const count = (o) => Object.values(res.boxes).filter((b) => b.orient === o).length;
  ok(count('down') >= 1 && count('right') >= 1 && count('left') >= 1, `south, east and west used: ${JSON.stringify(res.boxes)}`);
  ok(count('up') > 0, 'overflow reached the north arc');
  const order = res.rings.inner;
  const firstUp = order.findIndex((id) => res.boxes[id].orient === 'up');
  const lastSide = Math.max(...order.map((id, i) => (['left', 'right'].includes(res.boxes[id].orient) ? i : -1)));
  ok(firstUp > lastSide, `north only after east/west (${firstUp} vs ${lastSide})`);
}
{
  // No hub → grid of compound boxes with the tree inside.
  const input = fixture({ hub: false });
  const res = G.hubLayout(input);
  ok(res.mode === 'grid' && res.hubs.length === 0, `mode: ${res.mode}`);
  checkLayout('grid', input, res);
}
{
  // Empty input and an unrelated empty namespace.
  const res = G.hubLayout({ nodes: [], boxes: [{ id: 'ns:a', label: 'a' }], edges: [] });
  ok(res.mode === 'grid' && !!res.positions['ns:a'], 'empty namespace gets a position');
}
{
  // ~160 live nodes must stay well below the 200ms budget.
  const input = fixture({ hub: true, pad: 16 });
  const res = G.hubLayout(input);
  ok(res.ms < 200, `layout took ${res.ms}ms`);
}

// --- D-040 helpers ------------------------------------------------------------
{
  // Far tile priority: fail > warn > routing > normal.
  const w = [{ reason: 'x', message: 'm' }];
  ok(G.tileTone({ ok: false, warnings: w, kind: 'gateway' }) === 'fail', 'bad beats warn');
  ok(G.tileTone({ ok: true, warnings: w, kind: 'gateway' }) === 'warn', 'warn beats routing');
  ok(G.tileTone({ kind: 'virtualservice' }) === 'routing', 'routing tone');
  ok(G.tileTone({ kind: 'service', warnings: [] }) === '' && G.tileTone(null) === '', 'plain tone');
}
{
  const graph = {
    nodes: [
      { id: 'svc:production/mobile-api', kind: 'service', label: 'mobile-api', namespace: 'production' },
      { id: 'dep:production/mobile-api', kind: 'deployment', label: 'mobile-api', namespace: 'production' },
      { id: 'vs:production/mobile-api', kind: 'virtualservice', label: 'mobile-api', namespace: 'production' },
      { id: 'svc:util/zeta', kind: 'service', label: 'zeta', namespace: 'util',
        warnings: [{ reason: 'noBackend', message: 'no backend' }, { reason: 'noBackend', message: 'same reason again' }] },
      { id: 'svc:util/alpha', kind: 'service', label: 'alpha', namespace: 'util',
        warnings: [{ reason: 'targetPortNotDeclared', message: 'port <b>', hint: 'fix it', items: ['a', 'b'] }] },
      { id: 'vs:apps/web', kind: 'virtualservice', label: 'web', namespace: 'apps' },
      { id: 'ingc:nginx', kind: 'ingressclass', label: 'nginx', warnings: [{ reason: 'classNotFound', message: 'ghost' }] },
      { id: 'ns:production', kind: 'namespace', label: 'production' },
    ],
    edges: [
      { from: 'vs:apps/web', to: 'svc:production/mobile-api', label: 'routes', ok: false },
      { from: 'vs:production/mobile-api', to: 'svc:production/mobile-api', label: 'routes', ok: true },
    ],
  };
  const all = G.collectWarnings(graph);
  ok(all.total === 5 && all.shown === 5, `warning total: ${all.total}/${all.shown}`);
  ok(all.groups.map((g) => g.namespace).join(',') === ',apps,util', `ns order: ${all.groups.map((g) => g.namespace)}`);
  const util = all.groups.find((g) => g.namespace === 'util');
  ok(util.resources.map((r) => r.label).join(',') === 'alpha,zeta', 'resources sorted by name');
  const zeta = util.resources[1];
  ok(zeta.rows[0].key === 'svc:util/zeta|noBackend' && zeta.rows[1].key === 'svc:util/zeta|noBackend#2', `keys: ${zeta.rows.map((r) => r.key)}`);
  const broken = all.groups.find((g) => g.namespace === 'apps').resources[0].rows[0];
  ok(broken.type === 'broken' && broken.message.includes('production/mobile-api') && broken.key === 'vs:apps/web|brokenLink→svc:production/mobile-api', `broken row: ${JSON.stringify(broken)}`);
  ok(G.collectWarnings(graph, { ns: 'util' }).shown === 3, 'namespace filter');
  ok(G.collectWarnings(graph, { text: 'SAME reason' }).shown === 1, 'text filter on message');
  ok(G.collectWarnings(graph, { text: 'alpha' }).shown === 1, 'text filter on resource name');
  ok(G.collectWarnings(graph, { text: 'a' }).total === 5, 'total is unfiltered');
  ok(G.collectWarnings(null).total === 0, 'empty graph');

  // Search ranking: exact/prefix first, kind words, ns/name.
  const r = G.searchNodes(graph.nodes, 'mobile-api');
  ok(r.total === 3 && r.results.every((n) => n.namespace === 'production'), `mobile-api: ${r.results.map((n) => n.id)}`);
  ok(r.results.map((n) => n.kind).join(',') === 'virtualservice,service,deployment', `kind order: ${r.results.map((n) => n.kind)}`);
  ok(G.searchNodes(graph.nodes, 'MOBILE').total === 3, 'case-insensitive');
  ok(G.searchNodes(graph.nodes, 'mobile svc').results.map((n) => n.id).join() === 'svc:production/mobile-api', 'kind alias token');
  ok(G.searchNodes(graph.nodes, 'deployment mobile').total === 1, 'kind word token');
  ok(G.searchNodes(graph.nodes, 'util/').total === 2, 'ns/ prefix');
  ok(G.searchNodes(graph.nodes, 'production/mobile').total === 3, 'ns/name');
  const rank = G.searchNodes([
    { id: 'a', kind: 'service', label: 'my-api', namespace: 'x' },
    { id: 'b', kind: 'service', label: 'api-gw', namespace: 'x' },
    { id: 'c', kind: 'service', label: 'api', namespace: 'x' },
    { id: 'd', kind: 'service', label: 'rapid', namespace: 'x' },
  ], 'api');
  ok(rank.results.map((n) => n.id).join('') === 'cbad', `rank: ${rank.results.map((n) => n.id)}`);
  const many = [];
  for (let i = 0; i < 30; i++) many.push({ id: 'n' + i, kind: 'pod', label: 'pod-' + i, namespace: 'x' });
  const lim = G.searchNodes(many, 'pod', 12);
  ok(lim.results.length === 12 && lim.total === 30, `limit: ${lim.results.length}/${lim.total}`);
  ok(G.searchNodes(many, '  ').total === 0, 'blank query');

  // Host + test path fields.
  const q = (h, p) => JSON.stringify(G.traceQuery(h, p));
  ok(q('api.example.com', '') === JSON.stringify({ host: 'api.example.com', scheme: '', path: '/' }), `default path: ${q('api.example.com', '')}`);
  ok(G.traceQuery('api.example.com/api/x', '').path === '/api/x', 'pasted path used');
  ok(G.traceQuery('api.example.com/api/x', '/other').path === '/other', 'field wins when both set');
  ok(G.traceQuery('https://api.example.com:8443/a?b', 'x').path === '/x' && G.traceQuery('https://api.example.com:8443/a', '').scheme === 'https', 'url + relative field');
  ok(G.traceQuery('https://api.example.com:8443/a', '').host === 'api.example.com:8443', 'host keeps port');
  const sp = G.splitPastedHost('https://api.example.com/api/x', '');
  ok(sp && sp.host === 'https://api.example.com' && sp.path === '/api/x', `split: ${JSON.stringify(sp)}`);
  ok(G.splitPastedHost('api.example.com/api/x', '/keep').path === '/keep', 'split keeps an explicit field');
  ok(G.splitPastedHost('api.example.com', '') === null, 'nothing to split');
}

if (failed) process.exit(1);
console.log(`ok: chainOf/hubLayout/D-040 helpers (${file})`);
