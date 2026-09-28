#!/usr/bin/env node
// Record the real output of a sequence of commands, with real timestamps, as
// an asciicast v2 file (https://docs.asciinema.org/manual/asciicast/v2/).
//
//   node scripts/demo/record.mjs STEPS.json OUT.cast
//
// STEPS.json is { "title", "cols", "rows", "steps": [...] }. A step is either
//   { "show": "thesis run ...", "argv": ["..\\..\\bin\\thesis.exe", "run", ...],
//     "cwd": "testdata/kvfixture", "env": { "KV_VARIANT": "kvfixed" } }
// which runs a program and records everything it writes, or
//   { "show": "sed -i ... prothesis.yaml", "edit": { "file", "from", "to" } }
// which performs that one text replacement itself and prints nothing, as the
// command shown would.
//
// After each program step the recording adds `$ echo $?` and the exit code the
// program actually returned. Nothing is edited after the fact: the renderer may
// shorten long pauses when it plays a recording back, but the timestamps in
// this file are the ones measured here. The header's "prothesis" block records
// what really ran (argv, cwd, environment overrides, exit codes, host), because
// the command shown is written in POSIX form and the recording may have been
// made on Windows.

import { spawn } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { StringDecoder } from 'node:string_decoder';
import os from 'node:os';

const [stepsPath, outPath] = process.argv.slice(2);
if (!stepsPath || !outPath) {
  console.error('usage: node scripts/demo/record.mjs STEPS.json OUT.cast');
  process.exit(5);
}
const spec = JSON.parse(readFileSync(stepsPath, 'utf8'));
const t0 = process.hrtime.bigint();
const startedAt = new Date();
const events = [];
const log = [];

const now = () => Number(process.hrtime.bigint() - t0) / 1e9;
const emit = (text) => {
  if (text) events.push([Number(now().toFixed(6)), 'o', text.replace(/\r?\n/g, '\r\n')]);
};

function runProgram(step) {
  return new Promise((done) => {
    const cwd = resolve(step.cwd ?? '.');
    const env = { ...process.env, ...(step.env ?? {}) };
    const child = spawn(step.argv[0], step.argv.slice(1), { cwd, env, windowsHide: true });
    const out = new StringDecoder('utf8');
    const err = new StringDecoder('utf8');
    child.stdout.on('data', (d) => emit(out.write(d)));
    child.stderr.on('data', (d) => emit(err.write(d)));
    child.on('error', (e) => { emit(`recorder: could not start ${step.argv[0]}: ${e.message}\n`); done(127); });
    child.on('close', (code) => { emit(out.end()); emit(err.end()); done(code ?? 128); });
  });
}

for (const step of spec.steps) {
  emit(`$ ${step.show}\n`);
  if (step.edit) {
    const file = resolve(step.edit.file);
    const text = readFileSync(file, 'utf8');
    if (!text.includes(step.edit.from)) {
      emit(`recorder: ${step.edit.file} does not contain the text to replace\n`);
      log.push({ show: step.show, edit: step.edit, applied: false });
      continue;
    }
    writeFileSync(file, text.replace(step.edit.from, step.edit.to));
    log.push({ show: step.show, edit: step.edit, applied: true });
    continue;
  }
  const started = now();
  const code = await runProgram(step);
  log.push({ show: step.show, argv: step.argv, cwd: step.cwd ?? '.', env: step.env ?? {},
    exit_code: code, seconds: Number((now() - started).toFixed(3)) });
  emit('$ echo $?\n');
  emit(`${code}\n`);
}

const header = {
  version: 2,
  width: spec.cols ?? 100,
  height: spec.rows ?? 26,
  timestamp: Math.floor(startedAt.getTime() / 1000),
  title: spec.title ?? '',
  env: { TERM: 'xterm-256color', SHELL: 'recorded by scripts/demo/record.mjs' },
  prothesis: {
    recorded_at: startedAt.toISOString(),
    finished_at: new Date().toISOString(),
    host: `${os.type()} ${os.release()} ${os.arch()}`,
    note: 'Commands are shown in POSIX form; "steps" records what actually ran.',
    steps: log,
  },
};
writeFileSync(outPath, [JSON.stringify(header), ...events.map((e) => JSON.stringify(e))].join('\n') + '\n');
console.log(`recorded ${events.length} events over ${now().toFixed(1)} s -> ${outPath}`);
for (const s of log) {
  console.log(s.edit ? `  edit  ${s.edit.file} applied=${s.applied}` : `  exit ${s.exit_code}  ${s.seconds}s  ${s.show}`);
}
