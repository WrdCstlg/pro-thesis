# Pre-registration: the etcd target

**Written and frozen BEFORE any run against this target.** The reference
fixture proves the harness finds a bug it was built to find. This target asks
a different question: does the same unmodified checker discriminate between
a correct and an incorrect read mode on a system nobody here wrote?

## The two arms

Both arms use the same fault, the same seed handling and the same key space;
they differ only in what the driver asks etcd for on a read.

| Arm | Command (from `targets/etcd/`) | Expected exit |
|---|---|---|
| A: linearizable reads | `thesis run --profile linear --fault 'net.partition(etcd-n2)@3000..7500'` | `0` PASS |
| B: serializable reads | `thesis run --profile stale --fault 'net.partition(etcd-n2)@3000..7500'` | `1` FAIL, `linearizable.kv` |

**Why `etcd-n2` and not `role:leader`.** Role resolution needs the target's
status endpoint, which etcd does not expose in the shape the harness reads.
A fixed member is fine for the question being asked: whichever role `etcd-n2`
holds when the partition lands, it is cut from its peers on the peer plane and
still reachable by clients.

**Why the outcome is expected.** During the window, `etcd-n2` cannot reach a
quorum. A linearizable read on it must wait for one and times out, which the
driver records as `info`: no evidence either way. A serializable read on it
is answered from local state, which stops advancing the moment the member is
cut, while the other two members keep committing writes. A client that reads
from `etcd-n2` after another client's write was acknowledged elsewhere gets an
older value. That history is not linearizable, and the checker should say so.

## What would falsify each expectation

- **Arm B exits 0.** The partition did not isolate the peer plane, no
  serializable read landed on `etcd-n2` inside the window after a competing
  write, or the checker's search bound was hit and it refused (exit 2, not 0).
  Any of these is reported as measured. It is not evidence that serializable
  reads are linearizable.
- **Arm A exits 1.** Either a genuine etcd defect under default settings
  (an extraordinary claim that would need the witness examined by hand) or,
  far more likely, a driver classification error: an `ok` record for an
  operation whose outcome was actually unknown. The driver's classification
  table is the first suspect.
- **Either arm exits 2.** The oracle refused. The reason in the verdict is the
  finding.

## What is NOT claimed

Nothing here is a statement about etcd's correctness beyond the documented
semantics of its two read modes. `linearizable.kv` checks single-key register
histories; it says nothing about transactions, leases, watches or
multi-key operations.

## Budget, fixed in advance

Two worlds per arm (`profiles.linear.worlds` / `profiles.stale.worlds` are
`2`), 10 minutes wall per arm. The result is whatever those two worlds
produce; there is no re-running until it looks right.
