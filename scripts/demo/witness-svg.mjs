#!/usr/bin/env node
// Draw a FAIL verdict's witness: the fault window, and the operations the
// checker's search broke on, on one time axis.
//
//   node scripts/demo/witness-svg.mjs RUN_DIR/verdict.json OUT.svg
//
// Everything drawn comes from verdict.json: the causal timeline gives the times,
// the witness gives the operation ids, and the checker's explanation is quoted
// verbatim. Nothing is inferred beyond parsing those fields; a field that does
// not parse is left out rather than guessed.

import { readFileSync, writeFileSync } from 'node:fs';
import { basename, dirname } from 'node:path';

const [verdictPath, outPath] = process.argv.slice(2);
if (!verdictPath || !outPath) {
  console.error('usage: node scripts/demo/witness-svg.mjs RUN_DIR/verdict.json OUT.svg');
  process.exit(5);
}
const verdict = JSON.parse(readFileSync(verdictPath, 'utf8'));
const v = (verdict.violations ?? [])[0];
if (!v) {
  console.error(`${verdictPath}: no violation to draw`);
  process.exit(2);
}
const runId = verdict.run_id ?? basename(dirname(verdictPath));
const wanted = new Set((v.witness?.op_ids ?? []).map(Number));

// Operations and faults from the causal timeline. Only the first injected
// fault is drawn; its withdrawal is the first one that names the same spec,
// and the band says how many faults there were when there was more than one.
const ops = new Map();
let fault = null;
let faultCount = 0;
const OP = /process (\d+) (read|write) (\S+)(?: -> (\S+))? \[(invoke|ok|fail|info) op_id=(\d+)(?: error=(\S+))?\]/;
for (const e of v.causal_timeline ?? []) {
  if (e.event === 'op') {
    const m = OP.exec(e.detail);
    if (!m) continue;
    const id = Number(m[6]);
    if (!wanted.has(id)) continue;
    const op = ops.get(id) ?? { id, process: Number(m[1]), kind: m[2], key: m[3] };
    if (m[5] === 'invoke') {
      op.invoke = e.t_ms;
      if (m[2] === 'write') op.value = m[4];
    } else {
      op.end = e.t_ms;
      op.endType = m[5];
      op.error = m[7];
      if (m[2] === 'read') op.value = m[4];
      if (m[2] === 'write' && op.value === undefined) op.value = m[4];
    }
    ops.set(id, op);
  } else if (e.event === 'fault') {
    const inj = /^inject (\S+) -> (\S+)/.exec(e.detail);
    const wd = /^withdraw (\S+)/.exec(e.detail);
    if (inj) faultCount++;
    if (inj && !fault) fault = { spec: inj[1], node: inj[2], from: e.t_ms };
    if (wd && fault && fault.to === undefined && wd[1] === fault.spec) fault.to = e.t_ms;
  }
}

// The checker's own account: which value the register held, and which read
// contradicted it.
const ex = v.explanation ?? '';
const why = /At that point the register held (\S+), written by op (\d+), and op (\d+) \(process (\d+), read\) returned (\S+)\. That value was written by op (\d+), which every linearization places before the value the register held\./.exec(ex);
const count = /no linearization exists for the (\d+) operation/.exec(ex)?.[1];
const states = /exhausted \((\d+) state\(s\) explored over (\d+) step/.exec(ex);
const key = v.witness?.key ?? [...ops.values()][0]?.key ?? '?';

// Row order tells the story: the value written, the value that replaced it,
// then the read that returned the replaced one.
const order = why ? [Number(why[6]), Number(why[2]), Number(why[3])] : [...wanted];
const rows = order.map((id) => ops.get(id)).filter((o) => o && o.invoke !== undefined);
if (rows.length === 0) {
  console.error(`${verdictPath}: the causal timeline carries none of the witness operations`);
  process.exit(2);
}

// The one piece of arithmetic on the recorded times: how long after the
// replacing write completed the stale read was invoked. Real-time order is
// what makes the read stale, so it is stated, not left for the reader to find.
const replacer = why ? ops.get(Number(why[2])) : undefined;
const staleRead = why ? ops.get(Number(why[3])) : undefined;
const gap = replacer?.end !== undefined && staleRead?.invoke !== undefined && replacer.endType === 'ok'
  ? staleRead.invoke - replacer.end : undefined;

// Geometry.
const W = 920;
const LEFT = 188;
const RIGHT = 250;
const TOP = 132;
const ROWH = 44;
const times = rows.flatMap((o) => [o.invoke, o.end ?? o.invoke]).concat(fault ? [fault.from, fault.to ?? fault.from] : []);
const t0 = Math.floor((Math.min(...times) - 600) / 500) * 500;
const t1 = Math.ceil((Math.max(...times) + 600) / 500) * 500;
const x = (t) => LEFT + ((t - t0) / (t1 - t0)) * (W - LEFT - RIGHT);
const bandY = TOP;
const firstRowY = TOP + (fault ? 62 : 30);
const axisY = firstRowY + rows.length * ROWH + 4;
const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

function wrapText(s, width) {
  const words = s.split(' ');
  const lines = [];
  let cur = '';
  for (const w of words) {
    if ((cur + ' ' + w).trim().length > width) {
      lines.push(cur.trim());
      cur = w;
    } else cur += ' ' + w;
  }
  if (cur.trim()) lines.push(cur.trim());
  return lines;
}

const quote = why ? why[0] : '';
const quoteLines = quote ? wrapText(`“${quote}”`, 118) : [];
// When the write that replaced the value timed out, real-time order alone
// cannot show the read stale, and the diagram says what the checker did about
// it instead, but only when its explanation reports exploring both branches.
const bothBranches = /indeterminate write\(s\) explored on both the applied and the not-applied branch/.test(ex);
const gapLine = gap !== undefined && gap >= 0
  ? `op ${staleRead.id} was invoked ${gap} ms after op ${replacer.id} had completed, and returned the value op ${Number(why[6])} wrote.`
  : replacer?.endType === 'info' && bothBranches
    ? `op ${replacer.id} timed out, so the checker tried it both applied and not applied; neither gives this history a valid order.`
    : '';
const quoteY = axisY + 48 + (gapLine ? 30 : 0);
const H = quoteY + quoteLines.length * 18 + 36;

const ticks = [];
for (let t = Math.ceil(t0 / 1000) * 1000; t <= t1; t += 1000) {
  ticks.push(`<line class="grid" x1="${x(t)}" y1="${bandY - 6}" x2="${x(t)}" y2="${axisY}"/>` +
    `<text class="tick" x="${x(t)}" y="${axisY + 16}" text-anchor="middle">t+${(t / 1000).toFixed(0)} s</text>`);
}

const band = fault ? (() => {
  const a = Math.max(x(fault.from), LEFT);
  const b = Math.min(x(fault.to ?? t1), W - RIGHT);
  // The span is when the fault was actually in force, from the timeline; the
  // schedule it was asked for (the spec's @from..to) is left off to keep one
  // pair of times on the band, not two.
  const span = fault.to !== undefined ? `t+${fault.from}..${fault.to} ms` : `from t+${fault.from} ms`;
  const kind = fault.spec.replace(/\(.*$/, '');
  const what = kind === 'net.partition' ? `network partition: ${fault.node} cut off` : `${kind} on ${fault.node}`;
  const more = faultCount > 1 ? ` (first of ${faultCount} faults)` : '';
  return `<rect class="band" x="${a}" y="${bandY}" width="${Math.max(2, b - a)}" height="${axisY - bandY}" rx="3"/>` +
    `<text class="bandlabel" x="${a + 10}" y="${bandY + 20}">${esc(what + more)}</text>` +
    `<text class="bandspec" x="${a + 10}" y="${bandY + 37}">${esc(fault.spec.replace(/@.*$/, ''))} · ${esc(span)}</text>`;
})() : '';

const rowSvg = rows.map((o, i) => {
  const y = firstRowY + i * ROWH;
  const a = x(o.invoke);
  const b = x(o.end ?? o.invoke);
  const isRead = o.kind === 'read';
  const indeterminate = o.endType === 'info';
  const cls = isRead ? 'read' : indeterminate ? 'maybe' : 'write';
  const when = o.end !== undefined ? `t+${o.invoke}..${o.end} ms` : `invoked t+${o.invoke} ms`;
  const outcome = isRead ? `read returned ${o.value}`
    : indeterminate ? `write ${o.value} · timed out, may or may not have applied`
    : `write ${o.value} · ${o.endType === 'ok' ? 'ok' : o.endType}`;
  // A note is two lines, the outcome over its times. It goes after its bar;
  // if it would run off the canvas it goes before the bar, and if it fits on
  // neither side it stays after the bar rather than cover the row labels.
  const noteW = Math.max(outcome.length * 6.9, when.length * 6.7);
  const after = Math.max(a, b) + 10;
  const before = a - 10;
  const fitsAfter = after + noteW <= W - 20;
  const fitsBefore = before - noteW >= LEFT;
  const noteX = fitsAfter || !fitsBefore ? after : before;
  const anchor = noteX === after ? 'start' : 'end';
  return `<text class="rowlabel" x="${LEFT - 14}" y="${y + 5}" text-anchor="end">op ${o.id} · process ${o.process}</text>` +
    `<rect class="${cls}" x="${a}" y="${y - 9}" width="${Math.max(4, b - a)}" height="18" rx="4"/>` +
    `<text class="note ${isRead ? 'noteread' : ''}" x="${noteX}" y="${y}" text-anchor="${anchor}">${esc(outcome)}</text>` +
    `<text class="when" x="${noteX}" y="${y + 14}" text-anchor="${anchor}">${esc(when)}</text>`;
}).join('\n');

const title = `A stale read, caught: key ${key}`;
const sub = count
  ? `linearizable.kv exhausted its search of the ${count} operations on ${key}${states ? ` (${Number(states[1]).toLocaleString('en-US')} states)` : ''} and found no order a correct register could produce.`
  : 'linearizable.kv found no order of this key’s operations that a correct register could produce.';
const sub2 = 'Below: the three operations where its deepest attempt broke. They point at the failure; the proof is the whole search.';

const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-labelledby="wt wd">
<title id="wt">${esc(title)}</title>
<desc id="wd">${esc(`${sub} ${quote} Drawn from verdict.json of run ${runId}.`)}</desc>
<style>
:root{--bg:#ffffff;--fg:#1f2328;--muted:#59636e;--hair:#d1d9e0;--band:rgba(191,135,0,0.10);--bandedge:rgba(191,135,0,0.45);--bandfg:#7d4e00;--write:#0969da;--maybe:#9a6700;--read:#cf222e}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--muted:#9198a1;--hair:#30363d;--band:rgba(210,153,34,0.12);--bandedge:rgba(210,153,34,0.45);--bandfg:#e3b341;--write:#4493f8;--maybe:#d29922;--read:#ff7b72}}
text{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans",Helvetica,Arial,sans-serif;fill:var(--fg)}
.bg{fill:var(--bg);stroke:var(--hair)}.h1{font-size:19px;font-weight:600}.sub{font-size:13px;fill:var(--muted)}
.grid{stroke:var(--hair);stroke-width:1}.tick{font-size:11px;fill:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.band{fill:var(--band);stroke:var(--bandedge);stroke-width:1}.bandlabel{font-size:12px;fill:var(--bandfg);font-weight:600}
.bandspec{font-size:11px;fill:var(--bandfg);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.gap{font-size:13px;fill:var(--fg);font-weight:600}
.rowlabel{font-size:12.5px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;fill:var(--fg)}
.write{fill:var(--write)}.read{fill:var(--read)}.maybe{fill:none;stroke:var(--maybe);stroke-width:2;stroke-dasharray:5 4}
.note{font-size:12.5px;fill:var(--muted)}.noteread{fill:var(--read);font-weight:600}
.when{font-size:11px;fill:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.quote{font-size:12.5px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;fill:var(--fg)}.foot{font-size:11px;fill:var(--muted)}
</style>
<rect class="bg" x="0.5" y="0.5" width="${W - 1}" height="${H - 1}" rx="10"/>
<text class="h1" x="28" y="42">${esc(title)}</text>
<text class="sub" x="28" y="66">${esc(sub)}</text>
<text class="sub" x="28" y="86">${esc(sub2)}</text>
<text class="sub" x="28" y="106">Times are from the start of the client load (t+0), as the run recorded them.</text>
${ticks.join('\n')}
${band}
${rowSvg}
${gapLine ? `<text class="gap" x="28" y="${axisY + 48}">${esc(gapLine)}</text>` : ''}
${quoteLines.map((l, i) => `<text class="quote" x="28" y="${quoteY + i * 18}">${esc(l)}</text>`).join('\n')}
<text class="foot" x="28" y="${H - 18}">The checker’s words, quoted from verdict.json of run ${esc(runId)}. Drawn by scripts/demo/witness-svg.mjs.</text>
</svg>
`;
writeFileSync(outPath, svg);
console.log(`${outPath}: ${rows.length} operations${fault ? ', fault window' : ''}, ${W}x${H}`);
