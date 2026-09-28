#!/usr/bin/env node
// Play an asciicast v2 recording back as a self-contained animated SVG terminal.
//
//   node scripts/demo/render-cast-svg.mjs IN.cast OUT.svg
//        [--cols 100] [--rows 24] [--idle 1] [--page 5] [--hold 6] [--title "..."] [--static]
//
// Every line is drawn exactly as it was recorded. Three things change on
// playback, and the SVG's <desc> says so: pauses longer than --idle seconds are
// shortened to --idle; output that arrives all at once but is taller than the
// screen is shown a screen at a time, --page seconds per screen, instead of
// scrolling past unseen; and lines longer than --cols wrap. Colour is added:
// any ANSI in the recording is stripped, so every colour is classify()'s
// reading of the line's text, and the <desc> says that too. The recording keeps
// the measured timestamps. --static draws the final screen without animation,
// which is also what a reader who prefers reduced motion sees.

import { readFileSync, writeFileSync } from 'node:fs';
import { basename } from 'node:path';

const args = process.argv.slice(2);
const opt = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 ? args[i + 1] : dflt;
};
const positional = [];
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--static') continue;
  if (args[i].startsWith('--')) {
    i++;
    continue;
  }
  positional.push(args[i]);
}
const [inPath, outPath] = positional;
if (!inPath || !outPath) {
  console.error('usage: node scripts/demo/render-cast-svg.mjs IN.cast OUT.svg [--cols N] [--rows N] [--idle S] [--page S] [--hold S] [--title T] [--static | --at S]');
  process.exit(5);
}
const COLS = Number(opt('cols', 100));
const ROWS = Number(opt('rows', 24));
const IDLE = Number(opt('idle', 1));
const PAGE = Number(opt('page', 5));
const HOLD = Number(opt('hold', 6));
// --at S draws the screen as playback shows it S seconds in, without
// animation: a way to look at any moment of a recording as a still.
const AT = opt('at', undefined);
const STATIC = args.includes('--static') || AT !== undefined;

const raw = readFileSync(inPath, 'utf8').split('\n').filter(Boolean);
const header = JSON.parse(raw[0]);
const TITLE = opt('title', header.title || basename(inPath));
const events = raw.slice(1).map((l) => JSON.parse(l)).filter((e) => e[1] === 'o');

// Shorten long pauses. Only the playback clock moves; the text does not.
const times = [];
{
  let prev = 0;
  let shift = 0;
  for (const [t] of events) {
    if (t - prev > IDLE) shift += t - prev - IDLE;
    times.push(t - shift);
    prev = t;
  }
}

// Replay the byte stream into logical lines, each stamped with the moment it
// was completed on screen.
const ANSI = /\x1b\[[0-9;?]*[ -/]*[@-~]/g;
const logical = [];
{
  let cur = '';
  events.forEach(([, , data], i) => {
    const s = data.replace(ANSI, '');
    for (let k = 0; k < s.length; k++) {
      const c = s[k];
      if (c === '\r') {
        if (s[k + 1] !== '\n') cur = '';
        continue;
      }
      if (c === '\n') {
        logical.push({ text: cur, t: times[i] });
        cur = '';
        continue;
      }
      cur += c === '\t' ? ' '.repeat(8 - (cur.length % 8)) : c;
    }
  });
  if (cur) logical.push({ text: cur, t: times.at(-1) ?? 0 });
}

// Colour by meaning, never by guesswork: only lines whose role is unambiguous
// get a class.
function classify(text, prev) {
  if (text.startsWith('$ ')) return 'cmd';
  if (prev === '$ echo $?' && /^\d+$/.test(text.trim())) {
    return { 0: 'ok', 1: 'bad', 2: 'warn', 3: 'warn', 4: 'drift', 5: 'bad' }[Number(text.trim())] ?? 'dim';
  }
  if (/Verdict:\s+PASS/.test(text)) return 'ok';
  if (/Verdict:\s+FAIL/.test(text)) return 'bad';
  if (/Verdict:\s+(INCONCLUSIVE|BUDGET)/.test(text)) return 'warn';
  if (/ORACLE DRIFT/.test(text)) return 'drift';
  if (/REPRODUCED/.test(text)) return 'bad';
  if (/WARNING/.test(text)) return 'warn';
  if (/^-+ .*VERDICT/.test(text)) return 'head';
  if (/reported a violation|\bviolated\b/.test(text)) return 'bad';
  if (/\[fault\]/.test(text)) return 'fault';
  if (/\[phase\]/.test(text)) return 'phase';
  if (/\[op\]/.test(text)) return 'op';
  if (/^\s*[-+] config /.test(text)) return 'drift';
  return '';
}

function wrap(text) {
  if (text.length <= COLS) return [text];
  const parts = [];
  let rest = text;
  while (rest.length > COLS) {
    let cut = rest.lastIndexOf(' ', COLS);
    if (cut < COLS * 0.5) cut = COLS;
    parts.push(rest.slice(0, cut));
    rest = '    ' + rest.slice(cut).replace(/^ +/, '');
  }
  parts.push(rest);
  return parts;
}

const visual = [];
{
  let prev = '';
  for (const { text, t } of logical) {
    const cls = classify(text, prev);
    wrap(text).forEach((part, k) => visual.push({ text: part, t, cls, cont: k > 0 }));
    prev = text;
  }
}

// A burst of output taller than the screen would scroll past in a single frame
// and never be seen, so it is revealed a screen at a time instead, keeping two
// lines of context above each screen. A burst is a run of lines each arriving
// within BURST seconds of the one before. Only the playback clock moves.
const BURST = 0.05;
let paged = 0;
{
  const step = ROWS - 2;
  let shift = 0;
  for (let i = 0; i < visual.length;) {
    let j = i + 1;
    while (j < visual.length && visual[j].t - visual[j - 1].t <= BURST) j++;
    const start = visual[i].t;
    const n = j - i;
    const chunks = n > ROWS ? Math.ceil(n / step) : 1;
    for (let k = i; k < j; k++) {
      visual[k].t = chunks > 1 ? start + shift + Math.floor((k - i) / step) * PAGE : visual[k].t + shift;
    }
    if (chunks > 1) paged++;
    shift += (chunks - 1) * PAGE;
    i = j;
  }
}

// Geometry.
const FONT = 13;
const CHAR = 7.83;
const LH = 18;
const PAD = 16;
const BAR = 34;
const W = Math.ceil(PAD * 2 + COLS * CHAR);
const H = BAR + PAD + ROWS * LH + PAD - 2;
const DURATION = (visual.at(-1)?.t ?? 0) + HOLD;
const pct = (t) => Math.min(100, (t / DURATION) * 100).toFixed(3);
const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const drawn = AT === undefined ? visual : visual.filter((v) => v.t <= Number(AT));
const finalOffset = Math.max(0, drawn.length - ROWS) * LH;

// Scroll keyframes: the window moves down one line whenever a line appears
// below its last row.
const scrollFrames = [];
{
  const seen = new Map();
  visual.forEach((v, i) => seen.set(v.t, i + 1));
  let last = -1;
  scrollFrames.push('0%{transform:translateY(0)}');
  for (const [t, count] of [...seen.entries()].sort((a, b) => a[0] - b[0])) {
    const off = Math.max(0, count - ROWS) * LH;
    if (off !== last) scrollFrames.push(`${pct(t)}%{transform:translateY(-${off}px)}`);
    last = off;
  }
}

// One reveal keyframe per distinct moment, shared by every line completed then.
const moments = [...new Set(visual.map((v) => v.t))].sort((a, b) => a - b);
const momentIndex = new Map(moments.map((t, i) => [t, i]));
const revealCss = moments
  .map((t, i) => (Number(pct(t)) < 0.001
    ? `@keyframes r${i}{0%,100%{opacity:1}}`
    : `@keyframes r${i}{0%{opacity:0}${pct(t)}%,100%{opacity:1}}`))
  .join('\n');

const lineSvg = drawn
  .map((v, i) => {
    const y = BAR + PAD + (i + 1) * LH - 5;
    const anim = STATIC ? '' : ` style="animation-name:r${momentIndex.get(v.t)}"`;
    const cls = ['l', v.cls].filter(Boolean).join(' ');
    const body = v.cls === 'cmd' && !v.cont
      ? `<tspan class="ps">$ </tspan>${esc(v.text.slice(2))}`
      : esc(v.text);
    return `<text class="${cls}" x="${PAD}" y="${y}"${anim}>${body}</text>`;
  })
  .join('\n');

const recorded = header.prothesis?.recorded_at ?? new Date(header.timestamp * 1000).toISOString();
const host = header.prothesis?.host ?? 'unknown host';
const desc = `Terminal recording made ${recorded} on ${host}. Every line is shown as recorded; ` +
  `on playback, pauses longer than ${IDLE} s are shortened, ` +
  (paged ? `output that arrived at once but is taller than the screen is shown a screen at a time (${PAGE} s per screen), ` : '') +
  `and long lines wrap. Colours are the renderer's, keyed off each line's text; any colour in the recording is dropped. ` +
  `The recording with its measured timestamps is ${basename(inPath)}.`;

const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-labelledby="t d">
<title id="t">${esc(TITLE)}</title>
<desc id="d">${esc(desc)}</desc>
<style>
text{font-family:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;font-size:${FONT}px;fill:#c9d1d9;white-space:pre}
.ps{fill:#3fb950}.cmd{fill:#f0f6fc;font-weight:600}.dim{fill:#8b949e}.op{fill:#8b949e}.phase{fill:#79c0ff}
.fault{fill:#d2a8ff}.ok{fill:#3fb950;font-weight:600}.bad{fill:#ff7b72;font-weight:600}.warn{fill:#e3b341;font-weight:600}
.drift{fill:#f778ba;font-weight:600}.head{fill:#f0f6fc;font-weight:600}.title{fill:#8b949e;font-size:12px}
${STATIC ? `.strip{transform:translateY(-${finalOffset}px)}` : `.l{opacity:0;animation-duration:${DURATION.toFixed(3)}s;animation-iteration-count:infinite;animation-timing-function:steps(1,end)}
.strip{animation:scroll ${DURATION.toFixed(3)}s steps(1,end) infinite}
@keyframes scroll{${scrollFrames.join('')}100%{transform:translateY(-${finalOffset}px)}}
${revealCss}
@media (prefers-reduced-motion:reduce){.l{animation:none;opacity:1}.strip{animation:none;transform:translateY(-${finalOffset}px)}}`}
</style>
<defs><clipPath id="screen"><rect x="${PAD - 6}" y="${BAR + PAD - 1}" width="${W - 2 * PAD + 12}" height="${ROWS * LH + 2}"/></clipPath></defs>
<rect x="0.5" y="0.5" width="${W - 1}" height="${H - 1}" rx="10" fill="#0d1117" stroke="#30363d"/>
<path d="M0.5 ${BAR}H${W - 0.5}" stroke="#30363d"/>
<circle cx="20" cy="${BAR / 2}" r="6" fill="#ff5f57"/><circle cx="40" cy="${BAR / 2}" r="6" fill="#febc2e"/><circle cx="60" cy="${BAR / 2}" r="6" fill="#28c840"/>
<text class="title" x="${W / 2}" y="${BAR / 2 + 4}" text-anchor="middle">${esc(TITLE)}</text>
<g clip-path="url(#screen)"><g class="strip">
${lineSvg}
</g></g>
</svg>
`;
writeFileSync(outPath, svg);
console.log(`${outPath}: ${visual.length} lines, ${DURATION.toFixed(1)} s playback, ` +
  `${paged} burst(s) paged, ${(svg.length / 1024).toFixed(1)} KB${AT !== undefined ? ` (still at ${AT} s)` : STATIC ? ' (static)' : ''}`);
