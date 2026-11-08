#!/usr/bin/env node
// Regression guard for the UI HTML escaper (qa D-038a F1): extracts esc()
// from ui/app.js, runs it on XSS payloads and fails when any HTML-special
// character survives. Usage: node scripts/check-ui-escape.mjs [path/to/app.js]
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';

const file = process.argv[2] || join(dirname(fileURLToPath(import.meta.url)), '..', 'ui', 'app.js');
const src = readFileSync(file, 'utf8');
const m = /function esc\(s\) \{[\s\S]*?\n\}/.exec(src);
if (!m) {
  console.error(`esc() not found in ${file}`);
  process.exit(1);
}
const esc = new Function(`${m[0]}; return esc;`)();
const payloads = [
  '<img src=x onerror="window.__xss=1">',
  "'><script>alert(1)</script>",
  '`${alert(1)}`',
  '&lt;already-escaped&gt;',
  'a & b " c \' d < e > f',
];
let failed = 0;
for (const p of payloads) {
  const out = esc(p);
  if (/[<>"'`]/.test(out) || /&(?!#\d+;)/.test(out)) {
    console.error(`FAIL: esc(${JSON.stringify(p)}) = ${JSON.stringify(out)}`);
    failed++;
  }
}
if (esc(null) !== '' || esc(42) !== '42' || esc('plain text') !== 'plain text') {
  console.error('FAIL: esc must stringify null/numbers and keep plain text');
  failed++;
}
// itemsHTML (warning/step host lists): every item and the key go through esc().
const mi = /function itemsHTML\(items, key, noun\) \{[\s\S]*?\n\}/.exec(src);
if (!mi) {
  console.error(`itemsHTML() not found in ${file}`);
  process.exit(1);
}
const itemsHTML = new Function('state', 'esc', `${mi[0]}; return itemsHTML;`)({ openItems: new Set() }, esc);
for (const p of payloads) {
  const html = itemsHTML([p, p], p);
  if (/<(?!\/?(details|summary|span|ul|li)\b)/i.test(html) || !html.includes(esc(p))) {
    console.error(`FAIL: itemsHTML(${JSON.stringify(p)}) = ${html}`);
    failed++;
  }
}
if (itemsHTML([], 'k') !== '' || itemsHTML(undefined, 'k') !== '') {
  console.error('FAIL: itemsHTML must render nothing without items');
  failed++;
}
if (!itemsHTML(['a'], 'k', 'path').includes('1 path') || !itemsHTML(['a', 'b'], 'k').includes('2 hosts')) {
  console.error('FAIL: itemsHTML noun');
  failed++;
}

// --- D-040 surfaces: Warnings panel, search dropdown, card header mark,
// node warnings and trace steps. Each renderer is extracted from app.js and
// fed a forged topology whose every string field is an XSS payload; the
// output may only contain the renderer's own tags and must never carry a
// payload verbatim.
const fn = (name) => {
  const re = new RegExp(`function ${name}\\([^)]*\\) \\{[\\s\\S]*?\\n\\}`);
  const found = re.exec(src);
  if (!found) {
    console.error(`${name}() not found in ${file}`);
    process.exit(1);
  }
  return found[0];
};
const require = createRequire(import.meta.url);
const WTGraph = require(resolve(dirname(file), 'graphkit.js'));
const kindMeta = /const KIND_META = (\{[\s\S]*?\n\});/.exec(src);
if (!kindMeta) {
  console.error('KIND_META not found');
  process.exit(1);
}
const KIND_META = new Function(`return ${kindMeta[1]};`)();
const ALLOWED = /<(?!\/?(div|span|b|details|summary|ul|li|button|option)\b)/i;
const bad = (html, p) => ALLOWED.test(html) || html.includes(p) || /&(?!#\d+;)/.test(html);
for (const p of payloads) {
  const els = {};
  const mk = () => ({
    innerHTML: '', textContent: '', className: '', value: '', dataset: {}, scrollTop: 0, style: {},
    classList: { add() {}, remove() {}, toggle() {} }, setAttribute() {}, querySelector() { return null; },
    getBoundingClientRect() { return { left: 0, bottom: 0, width: 200 }; },
  });
  const $ = (sel) => els[sel] || (els[sel] = mk());
  const warn = { reason: p, message: p, hint: p, items: [p] };
  const graph = {
    nodes: [
      { id: p, kind: p, label: p, namespace: p, warnings: [warn, warn] },
      { id: 'svc:' + p, kind: 'service', label: p + 'x', namespace: p, warnings: [warn] },
    ],
    edges: [{ from: p, to: 'svc:' + p, label: p, ok: false }],
  };
  const st = { clusterGraph: graph, warnOpen: true, warnHTML: '', openItems: new Set(), search: { results: [], total: 0, active: 0 } };
  const ctx = { state: st, $, esc, WTGraph, KIND_META };
  const run = (body, ret, extra = {}) => new Function(...Object.keys(ctx), ...Object.keys(extra), `${body}; return ${ret};`)(
    ...Object.values(ctx), ...Object.values(extra));
  // Warnings panel (+ its namespace filter options and count badge).
  $('#warn-text').value = '';
  $('#warn-ns').value = '';
  run(`${mi[0]}\n${fn('fillWarnNs')}\n${fn('renderWarnings')}`, 'renderWarnings')();
  for (const sel of ['#warn-list', '#warn-ns']) {
    if (bad(els[sel].innerHTML, p) || !els[sel].innerHTML) {
      console.error(`FAIL: ${sel} with payload ${JSON.stringify(p)}: ${els[sel].innerHTML}`);
      failed++;
    }
  }
  // Search dropdown.
  st.search = { results: graph.nodes, total: 30, active: 0 };
  run(`${fn('renderSearchDrop')}`, 'renderSearchDrop', { placeSearchDrop() {} })();
  if (bad(els['#search-drop'].innerHTML, p) || !els['#search-drop'].innerHTML.includes('search-row')) {
    console.error(`FAIL: search dropdown with payload ${JSON.stringify(p)}: ${els['#search-drop'].innerHTML}`);
    failed++;
  }
  // Node warnings (inspector / modal) and trace steps (pathNotRouted items).
  const wh = run(`${mi[0]}\n${fn('warningsHTML')}`, 'warningsHTML')([warn, warn], p);
  const step = { name: p, hop: p, status: 'warn', ref: p, reason: 'pathNotRouted', detail: p, hint: p, group: p, items: [p, p] };
  const th = run(`${mi[0]}\n${fn('stepClass')}\n${fn('stepMark')}\n${fn('traceStepHTML')}`, 'traceStepHTML')(step);
  for (const [what, html] of [['warningsHTML', wh], ['traceStepHTML', th]]) {
    if (bad(html, p)) {
      console.error(`FAIL: ${what} with payload ${JSON.stringify(p)}: ${html}`);
      failed++;
    }
  }
}

if (failed) process.exit(1);
console.log(`ok: esc() escapes ${payloads.length} payloads; itemsHTML, Warnings panel, search dropdown, warningsHTML and traceStepHTML too (${file})`);
