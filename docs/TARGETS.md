# Putting a system under the gate

How to point PRO-THESIS at a system it did not ship with. Every claim below was read out of the
code or out of a recorded artifact; where a figure appears, the command that produced it is next to
it. The worked example throughout is `targets/etcd`, which is upstream etcd v3.5.17 with nothing
added to it.

The harness is a **pinned tool**. Everything an integration needs lives with the target, not here.
If the harness has to change to accommodate a target, that is a finding about the harness and
belongs in `OPEN_QUESTIONS.md`, not in a patch that makes one target work.

---

## 1. What each side provides

| The harness gives you | You give the harness |
|---|---|
| The lifecycle: BOOT, SEED, DRIVE with PERTURB nested inside it (D-071), HEAL, QUIESCE, ASSERT, TEARDOWN | A Compose file, or an image reference, that boots your system |
| Fault injection, scheduling, withdrawal and a residue check | `prothesis.yaml`: nodes, probes, fault policy, profiles |
| Recording: history, telemetry, world file, verdict, per-world result | A **driver** that exercises your system and writes the history |
| Six built-in oracles and an external oracle protocol | The **oracles** that state your system's invariants |
| A tamper lock over the gate, and the exit-code contract | A committed `.prothesis/lock` |

`thesis init` creates `.prothesis/{oracles,regressions,runs}` and writes an annotated
`prothesis.yaml` plus starter oracle definitions (`cmd/thesis/init.go:49`). `oracles/` and
`regressions/` are committed; `runs/` is the only transient one.

---

## 2. The target may be an image you cannot build

`targets/etcd/docker-compose.yaml` names `image: quay.io/coreos/etcd:v3.5.17` and nothing else. The
harness never needs your source. If your project publishes an image per commit, the gate can pin a
tag and test the artifact rather than a tree, which keeps the two repositories independent.

The cost: `thesis bisect` walks **git revisions**, not image tags, so an image-pinned gate cannot
bisect automatically.

---

## 3. `prothesis.yaml`

`thesis init` writes a commented skeleton. `targets/etcd/prothesis.yaml` is the same file for a real
third-party system and is worth reading start to finish. The parts that decide whether an
integration works at all:

```yaml
harness:
  backend: compose
  file: docker-compose.yaml

  # Parallel worlds get their own compose project, but published ports are NOT
  # namespaced. Parameterise them and name the variables here, or two workers
  # race for one port (OQ-056).
  prefix_env: ETCD_PREFIX

  nodes:
    # id             what faults address, never the compose service name
    # service        the logical service; several nodes may share one
    # compose_service  the actual service in the compose file
    # port           the CONTAINER-side client port. DEFAULTS TO 8080, the
    #                fixture's. Set it or health probes hit the wrong port.
    # port_env       the compose variable carrying this node's host port
    - { id: etcd-n1, service: etcd, compose_service: etcd-n1, role_hint: replica, port: 2379, port_env: ETCD_PORT_N1 }

  health:
    # The node selector takes a glob, so one entry can cover a whole service.
    # {host} is 127.0.0.1; {port} is this node's PUBLISHED host port.
    - node: "etcd:*"
      probe: "http://{host}:{port}/health"
      timeout: 30s

  steady_state:
    # A host-side command (internal/control ExecSteadyState). It may talk to
    # published ports or exec into the cluster; the etcd target does the former
    # because the image ships no shell.
    probe: "./bin/etcdsteady --timeout 55s"
    timeout: 60s

  role_probe:
    # Required before role:leader / role:follower targets can bind. Prints one
    # JSON line per member saying which is leader. Lock-covered.
    probe: "./bin/etcdrole"
    timeout: 10s

driver:
  cmd: "./bin/etcdload --history {history_path} --seed {seed} --profile {profile}"
  profiles:                                   # the WORKLOAD shapes
    smoke: { clients: 4, ops: 500, mix: { read: 0.5, write: 0.5 } }
    linear: { clients: 16, ops: 60000, mix: { read: 0.5, write: 0.5 } }

perturber:
  budget: { max_concurrent_faults: 3, max_faults_per_world: 24 }
  allow: [net.partition, net.latency, net.loss, proc.kill, proc.pause]
  deny: [io.fill, io.latency, clock.skew]
  constraints:
    - "never partition more than minority of etcd"

oracles:
  dir: .prothesis/oracles
  builtin: [no_crash, no_panic_log, availability_after_heal, no_stuck_op]

profiles:                                     # the RUN shapes, what --profile names
  smoke: { budget: 90s, worlds: 2, driver_profile: smoke }
  linear: { budget: 10m, worlds: 2, driver_profile: linear }

artifacts: { dir: .prothesis/runs, retain_passing: 3, retain_failing: all }
```

Two things that catch everyone:

- **There are two `profiles` blocks and they are not the same thing.** `driver.profiles` are
  workload shapes. Top-level `profiles` are run shapes (budget, world count) and each names a
  `driver_profile`. `thesis run --profile linear` resolves against the top-level one.
- **`port` defaults to 8080.** That default is the reference fixture's. A target serving anywhere
  else must say so per node.

Declare only long-lived services as nodes: `no_crash` matches concrete node ids, so a one-shot init
container that exits is not a violation unless you declared it.

---

## 4. The driver

One driver process per world. Four placeholders are substituted into `driver.cmd`
(`internal/control/seams.go:35`):

| Placeholder | Value |
|---|---|
| `{history_path}` | where to append the history |
| `{seed}` | this **world's** seed, not the run's, unsigned decimal |
| `{profile}` | the **driver** profile name, not its contents. Read the numbers yourself |
| `{plan_path}` | the operation plan, also exported as `PROTHESIS_PLAN_PATH` (D-021) |

Also exported: `PROTHESIS_TARGETS`, `PROTHESIS_NODES`, `PROTHESIS_HOST`, `PROTHESIS_PORT_<NODE>`,
`PROTHESIS_RUN_ID`, `PROTHESIS_PROJECT_DIR`, `PROTHESIS_BACKEND`, `PROTHESIS_STDIN_CONTROL`.

Four rules, each of which exists because breaking it produced a false verdict here:

1. **Execute the plan verbatim** when one is given, or exit non-zero. A driver that improvises turns
   a shrink into fiction: the fixture driver once had plan mode as a doc comment and an unread
   field, and a whole reduction stage believed it (OQ-052).
2. **Fail loudly on a parameter you cannot honour.** Never fall back to a built-in default. A run
   that silently drove a different workload than the profile names is unusable as evidence.
3. **Honour the drain.** At QUIESCE the harness writes `{"cmd":"stop"}` on your stdin and then
   closes it; honouring **either** signal is enough. Stop issuing new operations, let in-flight ones
   finish. Without this, every operation still open when the driver dies looks wedged and
   `no_stuck_op` correctly refuses to judge (D-042, OQ-033).
4. **Exit non-zero when you could not drive**, so a broken driver is INCONCLUSIVE and never a quiet
   pass over a workload that never ran.

---

## 5. The history, and the rule the whole thing rests on

One JSON object per line. Operation records carry `t_ns` (Unix epoch nanoseconds), `process`,
`type`, `f`, `key`, `value`, `op_id` and `error` (`pkg/schema/history.go:51`).

| `type` | Meaning |
|---|---|
| `invoke` | the operation was sent |
| `ok` | it definitely took effect |
| `fail` | it definitely did **not** take effect |
| `info` | it may or may not have taken effect |

**A timeout is `info`, never `fail`.** A checker may require an `ok` write to be visible afterwards.
It may never require that of an `info`, and it may never assume an `info` did nothing. Get this
wrong and you get either false failures on ordinary timeouts or a green run over real data loss.
This is the single most important paragraph in this document.

**Write operation records only.** Phase markers are `{"t_ns":...,"type":"info","event":"phase",
"phase":"HEAL"}` and the harness writes them. A record carrying `phase` without `event` is rejected.

**Carry your domain facts in `meta`.** History parsing does not reject unknown fields, so anything
you add survives into the artifact for your own oracle to read. Measured, from a recorded etcd
world:

```json
{"t_ns":1789442762673427900,"process":0,"type":"invoke","f":"read","key":"k/6","op_id":9,"meta":{"target":"etcd-n3","read_mode":"serializable"}}
```

That is how the etcd target distinguishes a serializable read from a linearizable one while holding
both to the same checker.

**Record the evidence your oracles will need.** A final read-back sweep at the end of DRIVE,
recorded as ordinary operations, is how end-state facts reach an oracle at all.

**Check the file before you spend a world on it.**

```bash
thesis history verify path/to/history.jsonl
```

It applies every structural rule above to a file: no project, no Docker, no run. It reports
malformed lines, records that are neither a valid operation nor a valid marker, operations with no
`op_id`, an `op_id` naming two operations, a completion with no invoke, a completion that precedes
its own invoke, and timestamps that are not epoch nanoseconds. It also prints the counts it
measured, so a clean result is readable as a measurement rather than as a silence.

Exit `0` conforms, `1` a witnessed breach with its line number, `2` the history cannot be judged.
An operation left open at the end lands in that third category rather than the second: it does not
prove the file is wrong, it proves the history stops before the answer, and it is the usual
signature of a driver that did not honour the drain.

It cannot check the `info` rule. Whether a timeout was recorded as `info` rather than `fail` is a
claim about what your system did, and no reading of the file can settle it.

---

## 6. Oracles

Format, executable contract and the rules that make a finding believable:
[`ORACLE_DEFINITIONS.md`](ORACLE_DEFINITIONS.md). The integration-level points:

- An oracle is **any executable**. It reads one JSON object on stdin (`history_path`,
  `final_state_path`, `telemetry_path`, `world_path`, and the measured `phases`) and writes one on
  stdout. Exit `0` ok, `1` violated, `2` inconclusive.
- **Prefer oracles that read only the artifacts.** They are deterministic given a bundle and can be
  re-examined afterwards. An oracle runs with a curated environment, not your shell's, so ambient
  connection details will not be there.
- **`final_state.json` is thin on purpose**: schema, run id, phases, and per node its `id`,
  `service`, `reachable` and raw `status` (`internal/control/artifacts.go:66`). It is not a database
  dump. Everything else must arrive through the history.
- **Fail closed.** If you could not check, say `inconclusive`. Reporting ok because nothing happened
  is the one failure this project exists to prevent.
- Two of the six built-ins need affordances a third-party image may lack: `no_unbounded_queue` reads
  a `/status` endpoint and `resource_return_to_baseline` needs a shell in the container. Against
  etcd both are left out of `oracles.builtin`, because each would return INCONCLUSIVE on every world
  and turn every run into exit 2 (OQ-063).

---

## 7. Faults

`kind(target[, params])@start..end`, milliseconds from the start of DRIVE. `pkg/schema/fault_kinds.go`
registers **17 kinds in 6 families**: `net.partition`, `net.latency`, `net.loss`, `net.bandwidth`,
`net.duplicate`, `net.reorder`, `proc.kill`, `proc.pause`, `proc.restart`, `proc.slow`, `io.fill`,
`io.error`, `io.latency`, `clock.skew`, `clock.jump`, `mem.pressure`, `fd.exhaust`. **Five have ever
been realized in a recorded world** here: `net.latency`, `net.partition`, `net.loss`, `proc.pause`,
`proc.kill`. Start with those.

Targets: a node id, `role:leader` or `role:follower` (needs `harness.role_probe`), an edge `a<->b`
which is always bidirectional, `minority(g)`, `majority(g)`, `any(k, g)`, `group:*`.

Do not pair `proc.kill` with a durative `net.*` fault on the same node: the sidecar cannot rejoin a
dead container's network namespace, the fault cannot be withdrawn, and the world is unevaluable
(OQ-042). Network faults need a **privileged sidecar** in the node's network namespace, and Linux
containers.

---

## 8. The lock, and what it is for

`.prothesis/lock` hashes every oracle definition byte for byte plus exactly nine configuration keys,
which the lock file lists under `covers`: `harness.health`, `harness.role_probe`, `oracles.builtin`,
`oracles.dir`, `perturber.allow`, `perturber.budget`, `perturber.deny`, `profiles`, `search`. Every
run re-derives the digest before a container starts. A mismatch is exit **4**: not "a test failed"
but "someone changed the test". Moving it is deliberate, and the reason lands in the diff:

```bash
thesis oracles lock --reason "add the stuck-job oracle"
```

This is what lets the gate survive an autonomous agent working on the system under test. Two rules
go with it: a fix and a gate change never travel in the same commit, and CI runs
`thesis oracles verify` before any world.

Two holes the lock file states about itself, both of which matter to you:

- **`driver.profiles` is not covered** (OQ-026). Shrinking `ops` from 60000 to 10 weakens the gate
  and moves no digest. Review workload changes as gate changes.
- **The digest covers oracle definitions, not the executables they name** (D-060). A rebuilt or
  swapped checker is a warning in the verdict, never exit 4. Since D-060 each resolved program's
  SHA-256 is recorded outside the digest, so the change is visible rather than silent.

---

## 9. Running it, and running it in CI

```bash
thesis oracles verify                                     # the gate is the committed one
thesis history verify .prothesis/runs/r_*/world-*/history.jsonl   # the histories conform
thesis run --profile smoke                                # no faults: boot and teardown are sane
thesis run --profile linear --fault 'net.partition(etcd-n1)@3000..7500'
echo $?                                                   # 0 pass, 1 fail, 2 nothing proven, 4 gate moved
thesis regress                                            # replay the committed corpus
thesis diagnose                                           # every refusal attributes to a known problem (D-082)
```

Pin the tool so a harness release cannot silently change your verdicts. The tagged module resolves
publicly (`go list -m github.com/WrdCstlg/pro-thesis@v0.1.0-phase0` prints
`github.com/WrdCstlg/pro-thesis v0.1.0-phase0`):

```bash
go install github.com/WrdCstlg/pro-thesis/cmd/thesis@v0.1.0-phase0
```

### Running the harness as a container

You do not need Go, or this repository, to gate a project. The [`Dockerfile`](../Dockerfile) builds
an image carrying `thesis`, the reference checker, the docker CLI and compose v2. It drives the
**host's** daemon through a mounted socket and runs no daemon of its own.

```bash
docker build -t pro-thesis:dev .            # from a checkout of this repository

docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD:/project" -w /project \
  -e PROTHESIS_PROBE_HOST=host.docker.internal \
  pro-thesis:dev run --profile linear --fault 'net.partition(app)@3000..7500'
```

Three things about that command are contract rather than detail.

**The probe address changes.** Health probes go to published ports on the **host's** network
namespace. Inside a container, `127.0.0.1` is the container's own loopback, so the default is wrong
here and every world would come back INCONCLUSIVE with a health timeout that reads like the
target's fault. `PROTHESIS_PROBE_HOST` names the address the container can reach the host on:
`host.docker.internal` under Docker Desktop, the bridge gateway (usually `172.17.0.1`) on a Linux
engine. The image sets the Docker Desktop value by default. An unusable value is refused at
startup with CONFIG_ERROR rather than at the first probe (D-087).

**Your driver and your oracles are not in the image, and cannot be.** They are built from your
repository. They must exist under the mounted project and be runnable on `linux/amd64`. A Windows
`.exe` in `bin/` will not do: the driver command `./bin/yourdriver` resolves to an extensionless
Linux binary inside the container. This is the single most common reason a containerized run fails
where a host run succeeds.

**Compose build contexts work; bind mounts may not.** A build context is streamed by the client out
of the container's filesystem, so it works from any mount path. A bind-mount volume
(`- ./data:/data`) is resolved by the daemon against the **host** filesystem and needs that path to
exist on the host as written. Named volumes are unaffected.

One more, and it decides whether this works on your engine at all. Both targets here publish to
loopback explicitly, `ports: ["127.0.0.1:${KV_PORT_N1:-18081}:8080"]`, which is right for a harness
running on the host. Measured on Docker Desktop for Windows, a containerized harness still reaches
those ports at `host.docker.internal`, because Desktop proxies that path: five worlds booted and
every health probe passed. On a Linux engine the published socket is bound to the host's loopback
interface and a container on the bridge cannot reach it, so the target's compose has to publish on
all interfaces instead. That case is reasoned, not measured here.

Parallel lanes from inside a container are not supported yet: the pre-flight that checks whether a
host port is free binds in the caller's own network namespace, which inside a container is not the
host's, so it cannot answer (OQ-074). Run single-lane from a container.

**Assert the exit code, never read it.** `scripts/ci-run.sh WANT_EXIT run --profile ...` takes the
wanted code as its first argument, so a step expected to fail that passes is a finding, and so is
the reverse. It also prints what every oracle concluded in every world, with the evidence counts, so
a green step reads as a measurement rather than as a green step. It needs bash and `jq`.

Each world tears down with volumes removed, so a database that initialises on first boot
re-initialises every world. Measure one world before planning a campaign.

---

## 10. Definition of done for a new target

Three results, in this order. Anything less and a later green result means nothing.

1. `smoke` passes 3 of 3 with no faults, and the residue check finds nothing left behind.
2. One **pre-registered** world FAILs, reproducing a defect you already know is there, with the
   expected outcome written down before the run. `targets/etcd/.prothesis/PREREGISTRATION.md` is
   what that looks like: it predicts that the same unmodified checker passes etcd's linearizable
   profile and fails its serializable one under a peer-plane partition.
3. The same world passes once the defect is fixed, and fails again when the fix is reverted.

That is the shape every fix in this repository has to take, applied to the integration itself.

---

## 11. Known gaps that will affect you

- **`thesis doctor` is a declared verb with no implementation** (`cmd/thesis/main.go:172`: "`doctor`
  arrives with the determinism-readiness report"), so there is no environment self-check yet.
- **The confirmation gate is k of k** (OQ-054). A defect reproducing 7 times in 9, which is what a
  race looks like, cannot enter the regression corpus yet.
- **`thesis bisect` has unit tests and no recorded live run**, and walks git revisions, not tags.
- **The retention policy is enforced by nothing** (OQ-070). Bundles accumulate; `history.jsonl` is
  roughly three quarters of the bytes.
- **Image identity is stable only while the build cache lives** (OQ-068): the first world after a
  cache prune records a different `sut.images` digest for an unchanged Dockerfile.
- **Node logs are captured into the bundle.** If your system logs a connection string or a token, it
  lands in an artifact. Read a bundle before sharing it.
