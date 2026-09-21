package telemetry

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// realistic fixtures
//
// Every fixture below is shaped like the real thing: cgroup v2 as Docker
// Desktop's Linux VM presents it, and `docker stats --format json` as the CLI
// prints it. Synthetic round numbers would let a parser bug hide behind a
// convenient value.
// ---------------------------------------------------------------------------

// memoryStatV2Baseline is /sys/fs/cgroup/memory.stat for a Go service that has
// allocated 50 MiB of heap and read 300 MiB of files.
//
// anon (52428800) and file (314572800) DIFFER BY A FACTOR OF SIX on purpose.
// That gap is the whole reason MemoryMetrics separates RSSBytes from
// CurrentBytes, and it is what makes this fixture able to fail.
const memoryStatV2Baseline = `anon 52428800
file 314572800
kernel 14680064
kernel_stack 327680
pagetables 1179648
sec_pagetables 0
percpu 12288
sock 45056
vmalloc 0
shmem 4194304
zswap 0
zswapped 0
file_mapped 2097152
file_dirty 8192
file_writeback 0
swapcached 0
anon_thp 0
file_thp 0
shmem_thp 0
inactive_anon 51380224
active_anon 1048576
inactive_file 209715200
active_file 104857600
unevictable 0
slab_reclaimable 8388608
slab_unreclaimable 4194304
slab 12582912
workingset_refault_anon 0
workingset_refault_file 0
workingset_activate_anon 0
workingset_activate_file 0
workingset_restore_anon 0
workingset_restore_file 0
workingset_nodereclaim 0
pgscan 0
pgsteal 0
pgfault 148231
pgmajfault 27
pgactivate 3021
pgdeactivate 0
pglazyfree 0
pglazyfreed 0
thp_fault_alloc 0
thp_collapse_alloc 0`

// memoryStatV2AfterWrites is the SAME container after the driver's write
// workload. Anonymous memory is unchanged (the process leaked nothing) but
// the page cache has grown by 500 MiB, which the kernel reclaims lazily.
const memoryStatV2AfterWrites = `anon 52428800
file 838860800
kernel 14680064
kernel_stack 327680
pagetables 1179648
shmem 4194304
inactive_anon 51380224
active_anon 1048576
inactive_file 314572800
active_file 524288000
slab 12582912
pgfault 401882
pgmajfault 31`

const procStatusFixture = `Name:	kvnode
Umask:	0022
State:	S (sleeping)
Tgid:	1
Ngid:	0
Pid:	1
PPid:	0
TracerPid:	0
Uid:	0	0	0	0
Gid:	0	0	0	0
FDSize:	64
Groups:
NStgid:	1
NSpid:	1
NSpgid:	1
NSsid:	1
VmPeak:	  1310720 kB
VmSize:	  1244160 kB
VmLck:	       0 kB
VmPin:	       0 kB
VmHWM:	     62464 kB
VmRSS:	     49152 kB
RssAnon:	     45056 kB
RssFile:	      4096 kB
RssShmem:	         0 kB
VmData:	  1114112 kB
VmStk:	       132 kB
VmExe:	      5120 kB
VmLib:	         8 kB
VmPTE:	       216 kB
VmSwap:	         0 kB
Threads:	9
SigQ:	0/127590
voluntary_ctxt_switches:	2841
nonvoluntary_ctxt_switches:	17`

const cpuStatFixture = `usage_usec 4821334
user_usec 3120445
system_usec 1700889
nr_periods 412
nr_throttled 7
throttled_usec 91234`

// Byte values the fixtures above encode, named so an assertion reads as a
// claim about the world rather than as a magic number.
const (
	fixtureAnonBytes       = 52428800  // memory.stat `anon`: TRUE RSS
	fixturePageCacheBytes  = 314572800 // memory.stat `file`: page cache
	fixtureCurrentBytes    = 381681664 // memory.current: anon + cache + kernel
	fixturePID1RSSBytes    = 50331648  // /proc/1/status VmRSS (49152 kB)
	fixtureShmemBytes      = 4194304
	fixtureKernelBytes     = 14680064
	fixtureAfterCacheBytes = 838860800 // memory.stat `file` after the writes
	fixtureAfterCurrent    = 905969664 // memory.current after the writes
)

// fixtureSection renders one @@THESIS-delimited section exactly as readScript
// emits it. An empty body models a file the container could not read: the
// header is printed, `cat` writes nothing, and the section is empty rather
// than missing.
func fixtureSection(path, body string) string {
	if strings.TrimSpace(body) == "" {
		return readDelimiter + path + "\n"
	}
	return readDelimiter + path + "\n" + strings.TrimRight(body, "\n") + "\n"
}

// execOutputV2 assembles a whole batched read for a healthy cgroup v2 target.
func execOutputV2(memStat, memCurrent, cpuStat, pids, procStatus, fdCount string) string {
	var b strings.Builder
	b.WriteString(fixtureSection(pathMemoryStatV2, memStat))
	b.WriteString(fixtureSection(pathMemoryCurrent, memCurrent))
	b.WriteString(fixtureSection(pathMemorySwap, "0"))
	b.WriteString(fixtureSection(pathCPUStat, cpuStat))
	b.WriteString(fixtureSection(pathPidsCurrent, pids))
	b.WriteString(fixtureSection(pathMemoryStatV1, "")) // absent on a v2 host
	b.WriteString(fixtureSection(pathProcStatus, procStatus))
	b.WriteString(fixtureSection(sectionFDCount, fdCount))
	return b.String()
}

func healthyExecOutput() string {
	return execOutputV2(memoryStatV2Baseline, "381681664", cpuStatFixture, "14", procStatusFixture, "23")
}

// ---------------------------------------------------------------------------
// THE RSS SEMANTICS. Everything else in this file is secondary to this.
// ---------------------------------------------------------------------------

// rss_bytes must be cgroup v2 `anon` and nothing else.
//
// cgroup v2's memory.current is TOTAL CHARGED MEMORY INCLUDING PAGE CACHE.
// Reporting it as RSS would make `resource_return_to_baseline` fail on a
// perfectly healthy system, because page cache grows under a write workload and
// is reclaimed lazily. That is a systematic FALSE RESOURCE VIOLATION (the
// worst outcome this tool can produce) so the value is pinned here against a
// fixture where anon and memory.current differ by a factor of seven.
func TestRSSTracksAnonNotMemoryCurrent(t *testing.T) {
	s := &Sample{Node: "kv-n1"}
	applyMemory(s, parseSections(healthyExecOutput()))

	if s.Memory == nil {
		t.Fatal("no memory metrics at all from a readable cgroup v2 fixture")
	}
	rss, ok := s.RSS()
	if !ok {
		reason, _ := s.AbsentReason(MetricMemoryRSS)
		t.Fatalf("rss was reported ABSENT from a fixture whose memory.stat carries `anon`: %s", reason)
	}
	if rss != fixtureAnonBytes {
		switch rss {
		case fixtureCurrentBytes:
			t.Fatalf("rss_bytes = %d = memory.current. memory.current INCLUDES PAGE CACHE, which grows "+
				"under a write workload and is reclaimed lazily, so resource_return_to_baseline would "+
				"report a leak on a healthy system. rss_bytes must be memory.stat `anon` (%d).",
				rss, fixtureAnonBytes)
		case fixturePID1RSSBytes:
			t.Fatalf("rss_bytes = %d = /proc/1/status VmRSS. That is PID 1 alone, not the cgroup, so it "+
				"under-reports every multi-process container. rss_bytes must be memory.stat `anon` (%d).",
				rss, fixtureAnonBytes)
		case fixturePageCacheBytes:
			t.Fatalf("rss_bytes = %d = memory.stat `file`, which is page cache, not resident anonymous "+
				"memory. rss_bytes must be `anon` (%d).", rss, fixtureAnonBytes)
		default:
			t.Fatalf("rss_bytes = %d, want memory.stat `anon` = %d", rss, fixtureAnonBytes)
		}
	}
	if s.Memory.Source != SourceCgroupV2Anon {
		t.Fatalf("memory.source = %q, want %q; a reader must never have to guess which quantity "+
			"rss_bytes is", s.Memory.Source, SourceCgroupV2Anon)
	}

	// The page-cache-inclusive figures must still be CARRIED, under their own
	// names. Dropping them would leave a human unable to see that a rising
	// memory.current is cache rather than a leak.
	if s.Memory.CurrentBytes == nil || *s.Memory.CurrentBytes != fixtureCurrentBytes {
		t.Fatalf("current_bytes = %v, want %d carried separately from rss_bytes",
			derefI64(s.Memory.CurrentBytes), fixtureCurrentBytes)
	}
	if s.Memory.PageCacheBytes == nil || *s.Memory.PageCacheBytes != fixturePageCacheBytes {
		t.Fatalf("page_cache_bytes = %v, want %d", derefI64(s.Memory.PageCacheBytes), fixturePageCacheBytes)
	}
	if s.Memory.PID1RSSBytes == nil || *s.Memory.PID1RSSBytes != fixturePID1RSSBytes {
		t.Fatalf("pid1_rss_bytes = %v, want %d (49152 kB from /proc/1/status)",
			derefI64(s.Memory.PID1RSSBytes), fixturePID1RSSBytes)
	}
	if *s.Memory.CurrentBytes == *s.Memory.RSSBytes {
		t.Fatal("current_bytes equals rss_bytes; this fixture was built so they differ, so the two " +
			"quantities are being conflated somewhere")
	}
	if s.Memory.ShmemBytes == nil || *s.Memory.ShmemBytes != fixtureShmemBytes {
		t.Fatalf("shmem_bytes = %v, want %d", derefI64(s.Memory.ShmemBytes), fixtureShmemBytes)
	}
	if s.Memory.KernelBytes == nil || *s.Memory.KernelBytes != fixtureKernelBytes {
		t.Fatalf("kernel_bytes = %v, want %d", derefI64(s.Memory.KernelBytes), fixtureKernelBytes)
	}
	if _, absent := s.AbsentReason(MetricMemoryRSS); absent {
		t.Fatal("memory.rss_bytes was marked absent even though it was observed; an oracle would " +
			"return INCONCLUSIVE over a metric it actually has")
	}
}

// The oracle-level consequence, stated as an arithmetic property.
//
// `resource_return_to_baseline` subtracts a pre-DRIVE baseline from a
// post-QUIESCE reading. Under a write workload the page cache grows by hundreds
// of megabytes while anonymous memory does not move. If the oracle's input
// tracked memory.current it would see that cache as a leak on EVERY healthy
// run. This test asserts the subtraction that oracle performs comes out at zero.
func TestRSSReturnsToBaselineWhilePageCacheDoesNot(t *testing.T) {
	baseline := &Sample{Node: "kv-n1"}
	applyMemory(baseline, parseSections(healthyExecOutput()))

	afterWrites := &Sample{Node: "kv-n1"}
	applyMemory(afterWrites, parseSections(execOutputV2(
		memoryStatV2AfterWrites, "905969664", cpuStatFixture, "14", procStatusFixture, "23")))

	base, ok1 := baseline.RSS()
	post, ok2 := afterWrites.RSS()
	if !ok1 || !ok2 {
		t.Fatalf("rss absent on one of the two samples (baseline ok=%v, post ok=%v); the oracle "+
			"cannot compare what it was never given", ok1, ok2)
	}
	if growth := post - base; growth != 0 {
		t.Fatalf("rss grew by %d bytes across a workload that leaked nothing. The fixture's `anon` is "+
			"identical in both readings, so any growth here means rss_bytes is tracking something "+
			"page-cache-inclusive and resource_return_to_baseline will fire on healthy systems.", growth)
	}

	// The control: memory.current DID grow, substantially. If this assertion
	// ever fails, the fixture stopped modelling the hazard and the test above
	// stopped proving anything.
	cacheGrowth := *afterWrites.Memory.CurrentBytes - *baseline.Memory.CurrentBytes
	if cacheGrowth < 500<<20 {
		t.Fatalf("memory.current grew by only %d bytes; the fixture must model a large page-cache "+
			"increase or it does not exercise the false-positive it exists to prevent", cacheGrowth)
	}
	if *afterWrites.Memory.PageCacheBytes != fixtureAfterCacheBytes {
		t.Fatalf("page_cache_bytes = %d, want %d", *afterWrites.Memory.PageCacheBytes, fixtureAfterCacheBytes)
	}
	if *afterWrites.Memory.CurrentBytes != fixtureAfterCurrent {
		t.Fatalf("current_bytes = %d, want %d", *afterWrites.Memory.CurrentBytes, fixtureAfterCurrent)
	}
}

// cgroup v1's `rss` is the documented fallback, and it must be labelled as such
// so a reader knows which kernel interface produced the number.
func TestRSSFallsBackToCgroupV1RSS(t *testing.T) {
	v1 := `cache 314572800
rss 41943040
rss_huge 0
shmem 0
mapped_file 2097152
dirty 8192
writeback 0
pgpgin 123456
pgpgout 98765
total_cache 314572800
total_rss 41943040`

	out := fixtureSection(pathMemoryStatV2, "") + // unified files absent on a v1 host
		fixtureSection(pathMemoryCurrent, "") +
		fixtureSection(pathMemorySwap, "") +
		fixtureSection(pathCPUStat, cpuStatFixture) +
		fixtureSection(pathPidsCurrent, "14") +
		fixtureSection(pathMemoryStatV1, v1) +
		fixtureSection(pathProcStatus, procStatusFixture) +
		fixtureSection(sectionFDCount, "23")

	s := &Sample{Node: "kv-n1"}
	applyMemory(s, parseSections(out))

	rss, ok := s.RSS()
	if !ok {
		t.Fatal("cgroup v1 `rss` was not used; a v1 host would report no RSS at all and every " +
			"resource oracle would be INCONCLUSIVE there")
	}
	if rss != 41943040 {
		t.Fatalf("rss_bytes = %d, want cgroup v1 memory.stat `rss` = 41943040", rss)
	}
	if s.Memory.Source != SourceCgroupV1RSS {
		t.Fatalf("memory.source = %q, want %q so the reader knows this came from the v1 interface",
			s.Memory.Source, SourceCgroupV1RSS)
	}
	if s.Memory.PageCacheBytes == nil || *s.Memory.PageCacheBytes != 314572800 {
		t.Fatalf("page_cache_bytes = %v, want the v1 `cache` field 314572800",
			derefI64(s.Memory.PageCacheBytes))
	}
}

// ---------------------------------------------------------------------------
// 1. parseSections / keyValues / scalar
// ---------------------------------------------------------------------------

func TestParseSectionsSplitsRealisticOutput(t *testing.T) {
	r := parseSections(healthyExecOutput())

	for _, want := range []string{pathMemoryStatV2, pathMemoryCurrent, pathCPUStat, pathPidsCurrent,
		pathProcStatus, sectionFDCount} {
		if _, ok := r.section(want); !ok {
			t.Fatalf("section %q is missing or empty; the batched read is one exec per node per "+
				"interval, so a lost section costs that metric for the whole run", want)
		}
	}
	// An empty file must read as ABSENT, never as a section whose content is
	// the empty string: "the file did not exist" and "the file said 0" are
	// different facts and only one of them permits an oracle to conclude
	// anything.
	if body, ok := r.section(pathMemoryStatV1); ok {
		t.Fatalf("cgroup v1 memory.stat is empty on this host but section() reported it present "+
			"with body %q", body)
	}
	if _, ok := r.section("/sys/fs/cgroup/never.read"); ok {
		t.Fatal("a section that was never emitted reported present")
	}
	if got := strings.TrimSpace(mustSection(t, r, pathPidsCurrent)); got != "14" {
		t.Fatalf("pids.current section = %q, want \"14\"", got)
	}
	if !strings.Contains(mustSection(t, r, pathCPUStat), "usage_usec 4821334") {
		t.Fatal("cpu.stat section lost its usage_usec line")
	}
}

// The exec output is not guaranteed to be clean: a shell profile, a TTY
// warning, or a busybox notice can precede the first delimiter, and Windows
// tooling can introduce CR. Neither may corrupt a section.
func TestParseSectionsToleratesNoiseAndCRLF(t *testing.T) {
	noisy := "the input device is not a TTY\nsh: warning: setlocale failed\n" +
		strings.ReplaceAll(healthyExecOutput(), "\n", "\r\n")

	r := parseSections(noisy)
	s := &Sample{Node: "kv-n1"}
	applyMemory(s, r)
	applyCPU(s, r)
	applyTasks(s, r)

	rss, ok := s.RSS()
	if !ok || rss != fixtureAnonBytes {
		t.Fatalf("rss = %d ok=%v with CRLF line endings and a leading warning; preamble before the "+
			"first delimiter belongs to no section and a trailing CR must not defeat ParseInt",
			rss, ok)
	}
	if s.CPU == nil || s.CPU.UsageUS == nil || *s.CPU.UsageUS != 4821334 {
		t.Fatalf("cpu usage_usec = %v under CRLF", s.CPU)
	}
	if s.Tasks == nil || s.Tasks.OpenFDs == nil || *s.Tasks.OpenFDs != 23 {
		t.Fatalf("open_fds = %v under CRLF", s.Tasks)
	}
}

// A malformed section must neither panic nor silently produce 0. Producing 0
// is the specific failure that turns `resource_return_to_baseline` into a
// rubber stamp: 0 compared against 0 always "returns to baseline".
func TestMalformedSectionsYieldAbsenceNotZero(t *testing.T) {
	garbage := execOutputV2(
		"anon\nfile abc\nkernel 99999999999999999999999999\nshmem\t\n",
		"cannot read memory.current: Permission denied",
		"usage_usec\nuser_usec oops",
		"N/A",
		"Name: kvnode\nVmRSS: not-a-number kB\nThreads: many",
		"ls: /proc/1/fd: Permission denied")

	s := &Sample{Node: "kv-n1"}
	r := parseSections(garbage) // must not panic
	applyMemory(s, r)
	applyCPU(s, r)
	applyTasks(s, r)

	if rss, ok := s.RSS(); ok {
		t.Fatalf("rss_bytes = %d from a memory.stat whose `anon` line has no value; an unparseable "+
			"field must be ABSENT, never 0 and never a guess", rss)
	}
	if _, ok := s.AbsentReason(MetricMemoryRSS); !ok {
		t.Fatal("memory.rss_bytes is missing but no reason was recorded; an oracle can then only " +
			"report a bare INCONCLUSIVE that reads like a flake")
	}
	if s.Memory != nil {
		if s.Memory.CurrentBytes != nil {
			t.Fatalf("current_bytes = %d parsed out of a permission-denied message",
				*s.Memory.CurrentBytes)
		}
		if s.Memory.PageCacheBytes != nil {
			t.Fatalf("page_cache_bytes = %d parsed out of `file abc`", *s.Memory.PageCacheBytes)
		}
		if s.Memory.KernelBytes != nil {
			t.Fatalf("kernel_bytes = %d parsed out of a value that overflows int64",
				*s.Memory.KernelBytes)
		}
		if s.Memory.PID1RSSBytes != nil {
			t.Fatalf("pid1_rss_bytes = %d parsed out of `VmRSS: not-a-number kB`",
				*s.Memory.PID1RSSBytes)
		}
	}
	if s.CPU != nil {
		t.Fatalf("cpu metrics were produced from a cpu.stat with no parseable usage_usec: %+v", s.CPU)
	}
	if _, ok := s.AbsentReason(MetricCPU); !ok {
		t.Fatal("cpu is absent but no reason was recorded")
	}
	if s.Tasks != nil {
		t.Fatalf("task metrics were produced from unparseable files: %+v", s.Tasks)
	}
	if _, ok := s.AbsentReason(MetricTasks); !ok {
		t.Fatal("tasks is absent but no reason was recorded")
	}
}

func TestKeyValuesSkipsBadLinesWithoutZeroing(t *testing.T) {
	in := "anon 52428800\n" +
		"file\t314572800\n" + // tab separated, as some kernels print
		"kernel   14680064\n" + // padded
		"broken\n" + // key with no value
		"file_dirty abc\n" + // non-numeric
		"huge 99999999999999999999999999\n" + // overflows int64
		"\n" +
		"   \n" +
		"negative -5\n"

	kv := keyValues(in)

	for key, want := range map[string]int64{
		"anon": 52428800, "file": 314572800, "kernel": 14680064, "negative": -5,
	} {
		got, ok := kv[key]
		if !ok {
			t.Fatalf("key %q was dropped; one odd line in memory.stat must not blind the collector "+
				"to the rest of the file", key)
		}
		if got != want {
			t.Fatalf("key %q = %d, want %d", key, got, want)
		}
	}
	for _, key := range []string{"broken", "file_dirty", "huge"} {
		if v, ok := kv[key]; ok {
			t.Fatalf("unparseable key %q was admitted with value %d; a fabricated value is worse "+
				"than a missing one because nothing downstream can tell it apart from a measurement",
				key, v)
		}
	}
	if len(keyValues("")) != 0 {
		t.Fatal("empty input produced keys")
	}
}

func TestScalarDistinguishesZeroFromUnreadable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
		ok   bool
	}{
		{"pids.current", "14\n", 14, true},
		{"a genuine zero", " 0 \n", 0, true},
		{"padded", "\t 4096\t\n", 4096, true},
		{"memory.max is not a number", "max\n", 0, false},
		{"empty file", "", 0, false},
		{"whitespace only", "  \n\t\n", 0, false},
		{"two fields", "12 34\n", 0, false},
		{"error text", "cat: can't open: No such file\n", 0, false},
		{"float", "1.5\n", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scalar(tc.in)
			if ok != tc.ok {
				t.Fatalf("scalar(%q) ok = %v, want %v. A file that said 0 and a file that could not "+
					"be read must be distinguishable: only one of them lets an oracle conclude "+
					"anything.", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("scalar(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestProcStatusParsers(t *testing.T) {
	if got, ok := procStatusKB(procStatusFixture, "VmRSS"); !ok || got != fixturePID1RSSBytes {
		t.Fatalf("VmRSS = %d ok=%v, want %d bytes (49152 kB); the kB suffix must be converted or "+
			"pid1_rss_bytes is off by 1024x", got, ok, fixturePID1RSSBytes)
	}
	if got, ok := procStatusInt(procStatusFixture, "Threads"); !ok || got != 9 {
		t.Fatalf("Threads = %d ok=%v, want 9", got, ok)
	}
	if _, ok := procStatusKB(procStatusFixture, "VmNope"); ok {
		t.Fatal("a field /proc/1/status does not carry was reported present")
	}
	if _, ok := procStatusInt("Threads:\n", "Threads"); ok {
		t.Fatal("`Threads:` with no value was accepted; that would report 0 threads for a live process")
	}
	if _, ok := procStatusKB("VmRSS:\tlots kB\n", "VmRSS"); ok {
		t.Fatal("a non-numeric VmRSS was accepted")
	}
}

// ---------------------------------------------------------------------------
// 2. absent versus zero
// ---------------------------------------------------------------------------

// The design requires an unobservable metric to be ABSENT, not 0, so that an
// oracle needing it returns INCONCLUSIVE rather than PASS. This test drives
// that property all the way through the wire format, because the failure it
// guards against is a JSON `0` where a `null` belonged.
func TestUnobservableMetricsStayAbsentThroughTheWireFormat(t *testing.T) {
	// A cgroup v2 host whose memory.stat carries no `anon` (it exists: some
	// nested cgroup configurations report only the aggregate), no cpu.stat at
	// all, and no listable /proc/1/fd.
	out := fixtureSection(pathMemoryStatV2, "file 314572800\nshmem 4194304\n") +
		fixtureSection(pathMemoryCurrent, "381681664") +
		fixtureSection(pathMemorySwap, "0") +
		fixtureSection(pathCPUStat, "") +
		fixtureSection(pathPidsCurrent, "14") +
		fixtureSection(pathMemoryStatV1, "") +
		fixtureSection(pathProcStatus, procStatusFixture) +
		fixtureSection(sectionFDCount, "")

	s := &Sample{Node: "kv-n1", Seq: 7}
	r := parseSections(out)
	applyMemory(s, r)
	applyCPU(s, r)
	applyTasks(s, r)

	if rss, ok := s.RSS(); ok {
		t.Fatalf("rss_bytes = %d with no `anon` in memory.stat. memory.current (381681664) and "+
			"docker stats MemUsage both include page cache and must NOT be substituted.", rss)
	}
	if s.CPU != nil {
		t.Fatalf("cpu metrics present with an empty cpu.stat: %+v", s.CPU)
	}
	if s.Tasks == nil || s.Tasks.OpenFDs != nil {
		t.Fatalf("open_fds = %v with no listable /proc/1/fd; it must be absent, not 0, or "+
			"an fd-leak oracle compares 0 against 0 and always passes", s.Tasks)
	}
	if s.Tasks.PIDsCurrent == nil || *s.Tasks.PIDsCurrent != 14 {
		t.Fatal("pids.current was readable and must still be reported; one absent metric must not " +
			"take its whole group down with it")
	}

	for _, m := range []string{MetricMemoryRSS, MetricCPU, MetricTasksOpenFDs} {
		reason, ok := s.AbsentReason(m)
		if !ok {
			t.Fatalf("%s is unobserved but carries no reason; the difference between "+
				"\"inconclusive\" and \"inconclusive: cpu.stat was not readable\" is the whole "+
				"point of the Absent list", m)
		}
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("%s carries an empty reason", m)
		}
	}
	if reason, _ := s.AbsentReason(MetricMemoryRSS); !strings.Contains(reason, "anon") {
		t.Fatalf("the memory.rss_bytes absence reason %q does not name `anon`, so a reader cannot "+
			"tell what was looked for", reason)
	}

	// Through the wire format. A `0` here is the regression this test exists
	// to stop.
	line, err := s.MarshalLine()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(line), `"rss_bytes":null`) {
		t.Fatalf("serialized sample does not carry \"rss_bytes\":null.\n%s", line)
	}
	if strings.Contains(string(line), `"rss_bytes":0`) {
		t.Fatalf("serialized sample carries \"rss_bytes\":0 for a metric that was never observed; "+
			"an oracle reading it would compare 0 against 0 and PASS over an unmeasured system.\n%s",
			line)
	}
	if !strings.Contains(string(line), `"cpu":null`) {
		t.Fatalf("an unobserved cpu group must serialize as null, not as an empty object.\n%s", line)
	}

	back, err := ParseSample(line)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if _, ok := back.RSS(); ok {
		t.Fatal("rss became observable after a JSON round trip")
	}
	if back.CPU != nil {
		t.Fatal("cpu became non-nil after a JSON round trip")
	}
	if reason, ok := back.AbsentReason(MetricMemoryRSS); !ok || reason == "" {
		t.Fatal("the absence reason did not survive the round trip, so the oracle input loses its " +
			"diagnosis")
	}
}

// The control for the test above: a metric whose observed value happens to be
// zero must be PRESENT. Without this, "everything is absent" would pass the
// absent-versus-zero test vacuously.
func TestAnObservedZeroIsPresentNotAbsent(t *testing.T) {
	out := execOutputV2(
		"anon 0\nfile 0\nshmem 0\nkernel 0\n",
		"0", "usage_usec 0\nuser_usec 0\nsystem_usec 0\n", "0",
		"VmRSS:\t       0 kB\nThreads:\t0\n", "0")

	s := &Sample{Node: "kv-n1"}
	r := parseSections(out)
	applyMemory(s, r)
	applyCPU(s, r)
	applyTasks(s, r)

	rss, ok := s.RSS()
	if !ok {
		t.Fatal("a cgroup that genuinely reports anon=0 must be OBSERVED at zero, not reported " +
			"absent; the format has to be able to express a real zero or the absence signal is " +
			"meaningless")
	}
	if rss != 0 {
		t.Fatalf("rss = %d, want 0", rss)
	}
	if _, absent := s.AbsentReason(MetricMemoryRSS); absent {
		t.Fatal("an observed zero was also recorded as absent")
	}
	if s.CPU == nil || s.CPU.UsageUS == nil || *s.CPU.UsageUS != 0 {
		t.Fatalf("cpu usage_usec 0 must be observed, got %+v", s.CPU)
	}
	if s.Tasks == nil || s.Tasks.OpenFDs == nil || *s.Tasks.OpenFDs != 0 {
		t.Fatalf("open_fds 0 must be observed, got %+v", s.Tasks)
	}
	line, err := s.MarshalLine()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(line), `"rss_bytes":0`) {
		t.Fatalf("an observed zero did not serialize as 0.\n%s", line)
	}
}

// A whole metric group must disappear rather than materialise empty when
// nothing in it was readable.
func TestUnreadableGroupsBecomeNilNotEmptyStructs(t *testing.T) {
	out := fixtureSection(pathMemoryStatV2, "") +
		fixtureSection(pathMemoryCurrent, "") +
		fixtureSection(pathMemorySwap, "") +
		fixtureSection(pathCPUStat, "") +
		fixtureSection(pathPidsCurrent, "") +
		fixtureSection(pathMemoryStatV1, "") +
		fixtureSection(pathProcStatus, "") +
		fixtureSection(sectionFDCount, "")

	s := &Sample{Node: "kv-n1"}
	r := parseSections(out)
	applyMemory(s, r)
	applyCPU(s, r)
	applyTasks(s, r)

	if s.Memory != nil {
		t.Fatalf("memory group is non-nil with nothing readable: %+v; observedMetrics would then "+
			"count `memory` as present", s.Memory)
	}
	if s.CPU != nil {
		t.Fatalf("cpu group is non-nil with nothing readable: %+v", s.CPU)
	}
	if s.Tasks != nil {
		t.Fatalf("tasks group is non-nil with nothing readable: %+v", s.Tasks)
	}
	for _, m := range []string{MetricMemory, MetricMemoryRSS, MetricCPU, MetricTasks, MetricTasksOpenFDs} {
		if _, ok := s.AbsentReason(m); !ok {
			t.Fatalf("%s has no recorded absence reason on a target whose cgroup could not be read "+
				"at all", m)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. deriveCPURate
// ---------------------------------------------------------------------------

// A rate needs two samples, a counter reset must not become a spike, and an
// idle container must report a real 0.
//
// The expected milli-percent values below are worked out from the definition
// (cpu-seconds per wall-second x 100 x 1000), not from the implementation's
// expression, so a change to that expression fails here.
func TestDeriveCPURate(t *testing.T) {
	const ms = int64(1_000_000) // ns per ms

	sample := func(rtNS int64, usage *int64) *Sample {
		s := &Sample{Node: "kv-n1", RTNS: rtNS}
		if usage != nil {
			s.CPU = &CPUMetrics{UsageUS: i64(*usage)}
		}
		return s
	}

	cases := []struct {
		name       string
		prev       *Sample
		cur        *Sample
		wantMilli  *int64
		wantDelta  *int64
		wantReason string
	}{
		{
			name:       "first sample for this node has no rate",
			prev:       nil,
			cur:        sample(0, i64(4821334)),
			wantReason: "two samples",
		},
		{
			name:       "previous sample carried no cpu at all",
			prev:       sample(0, nil),
			cur:        sample(500*ms, i64(4821334)),
			wantReason: "two samples",
		},
		{
			// 250 ms of CPU over 500 ms of wall = half a core.
			name:      "half a core over the default interval",
			prev:      sample(0, i64(4821334)),
			cur:       sample(500*ms, i64(4821334+250000)),
			wantMilli: i64(50000),
			wantDelta: i64(250000),
		},
		{
			// 1 s of CPU over 1 s of wall = one core saturated = 100.000%.
			name:      "one core saturated",
			prev:      sample(0, i64(0)),
			cur:       sample(1000*ms, i64(1_000_000)),
			wantMilli: i64(100000),
			wantDelta: i64(1_000_000),
		},
		{
			// This host has 8 CPUs; 800000 milli-pct is the ceiling a real
			// container can reach and must not overflow or wrap.
			name:      "eight cores saturated",
			prev:      sample(0, i64(0)),
			cur:       sample(1000*ms, i64(8_000_000)),
			wantMilli: i64(800000),
			wantDelta: i64(8_000_000),
		},
		{
			name:      "an idle container reports a real zero",
			prev:      sample(0, i64(4821334)),
			cur:       sample(500*ms, i64(4821334)),
			wantMilli: i64(0),
			wantDelta: i64(0),
		},
		{
			// proc.restart recreates the container, so the cgroup counter
			// restarts from near zero. A rate across that boundary would be a
			// large negative number, which downstream reads as a impossible
			// measurement rather than as a restart.
			name:       "counter went backwards after a container restart",
			prev:       sample(0, i64(4821334)),
			cur:        sample(500*ms, i64(1205)),
			wantReason: "backwards",
		},
		{
			name:       "counter reset exactly to zero",
			prev:       sample(0, i64(4821334)),
			cur:        sample(500*ms, i64(0)),
			wantReason: "backwards",
		},
		{
			name:       "no monotonic time passed between samples",
			prev:       sample(1000*ms, i64(4821334)),
			cur:        sample(1000*ms, i64(4900000)),
			wantReason: "interval",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deriveCPURate(tc.cur, tc.prev)

			got := tc.cur.CPU.MilliPct
			switch {
			case tc.wantMilli == nil && got != nil:
				t.Fatalf("milli_pct = %d where no rate is derivable. A fabricated rate here feeds "+
					"the Saboteur's REINFORCE classifier and a resource oracle; it must be absent.",
					*got)
			case tc.wantMilli != nil && got == nil:
				reason, _ := tc.cur.AbsentReason(MetricCPUMilliPct)
				t.Fatalf("milli_pct is absent (%q) where %d was derivable from two good samples",
					reason, *tc.wantMilli)
			case tc.wantMilli != nil && *got != *tc.wantMilli:
				t.Fatalf("milli_pct = %d, want %d (milli-percent is percent x 1000, so one "+
					"saturated core is 100000)", *got, *tc.wantMilli)
			}

			if tc.wantDelta != nil {
				if tc.cur.CPU.DeltaUS == nil || *tc.cur.CPU.DeltaUS != *tc.wantDelta {
					t.Fatalf("delta_us = %v, want %d", derefI64(tc.cur.CPU.DeltaUS), *tc.wantDelta)
				}
				if tc.cur.CPU.IntervalNS == nil || *tc.cur.CPU.IntervalNS <= 0 {
					t.Fatalf("interval_ns = %v; the rate's denominator must be recorded so a "+
						"reader can check the arithmetic", derefI64(tc.cur.CPU.IntervalNS))
				}
			} else {
				if tc.cur.CPU.DeltaUS != nil || tc.cur.CPU.IntervalNS != nil {
					t.Fatalf("delta_us=%v interval_ns=%v were filled in for a sample with no "+
						"derivable rate", derefI64(tc.cur.CPU.DeltaUS), derefI64(tc.cur.CPU.IntervalNS))
				}
			}

			if tc.wantReason != "" {
				reason, ok := tc.cur.AbsentReason(MetricCPUMilliPct)
				if !ok {
					t.Fatal("no absence reason for cpu.milli_pct; a missing rate with no " +
						"explanation is indistinguishable from a collector bug")
				}
				if !strings.Contains(reason, tc.wantReason) {
					t.Fatalf("absence reason %q does not mention %q", reason, tc.wantReason)
				}
			} else if _, ok := tc.cur.AbsentReason(MetricCPUMilliPct); ok {
				t.Fatal("a derived rate was also marked absent")
			}
		})
	}
}

// A restart must never produce a wild value in EITHER direction. Stated
// separately from the table because it is the property, not the mechanism.
func TestCounterResetProducesNoRateAtAll(t *testing.T) {
	prev := &Sample{Node: "kv-n1", RTNS: 0, CPU: &CPUMetrics{UsageUS: i64(120_000_000)}}
	cur := &Sample{Node: "kv-n1", RTNS: 500_000_000, CPU: &CPUMetrics{UsageUS: i64(3_400)}}

	deriveCPURate(cur, prev)

	if cur.CPU.MilliPct != nil {
		v := *cur.CPU.MilliPct
		t.Fatalf("a cgroup counter reset produced milli_pct = %d. Negative CPU is impossible and a "+
			"huge positive would look like a saturation event that never happened; either would "+
			"drive the Saboteur toward a phantom finding.", v)
	}
	if cur.CPU.UsageUS == nil || *cur.CPU.UsageUS != 3400 {
		t.Fatal("the raw cumulative counter must still be recorded across a restart; only the " +
			"DERIVED rate is undefined there")
	}
}

// ---------------------------------------------------------------------------
// 4. docker stats string formats
// ---------------------------------------------------------------------------

func TestParseBytesAgainstDockerFormats(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		// The binary units docker actually prints.
		{"0B", 0, true},
		{"408KiB", 417792, true},
		{"1.5MiB", 1572864, true},
		{"365.1MiB", 382835098, true},
		{"1.234GiB", 1324997411, true},
		{"30.05GiB", 32265941811, true},
		{"2GiB", 2147483648, true},
		{"1TiB", 1099511627776, true},
		{"1PiB", 1125899906842624, true},
		// SI units, which docker uses for block and network IO.
		{"1kB", 1000, true},
		{"1KB", 1000, true},
		{"12.34MB", 12340000, true},
		{"1.234GB", 1234000000, true},
		{"1TB", 1000000000000, true},
		// Whitespace, as it arrives from splitting "408KiB / 30.05GiB".
		{" 408KiB ", 417792, true},
		{"1.5 MiB", 1572864, true},
		// Malformed. Each must be REFUSED, not coerced to 0: a coerced 0 is
		// indistinguishable from a container using no memory.
		{"", 0, false},
		{"   ", 0, false},
		{"abc", 0, false},
		{"GiB", 0, false},
		{"MiB", 0, false},
		{"1.2.3GiB", 0, false},
		{"--", 0, false},
		{"1.5Mi", 0, false},
		{"12", 0, false},
		{"1 234KiB", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseBytes(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseBytes(%q) ok = %v, want %v. An unparseable size must be absent; "+
					"reporting 0 bytes for a container docker could not measure would let a "+
					"memory oracle conclude it is idle.", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseBytes(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// The unit table is ordered longest-suffix-first for a reason: "1MB" ends in
// "B", and a shortest-match scan would read it as 1 byte.
func TestParseBytesPrefersTheLongestUnitSuffix(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"1B", 1},
		{"1kB", 1000},
		{"1MB", 1000000},
		{"1GB", 1000000000},
		{"1KiB", 1024},
		{"1MiB", 1048576},
		{"1GiB", 1073741824},
	} {
		got, ok := parseBytes(tc.in)
		if !ok || got != tc.want {
			t.Fatalf("parseBytes(%q) = %d ok=%v, want %d. If a shorter suffix matched first, "+
				"1MiB would be read as 1 byte and a memory series would be three orders of "+
				"magnitude wrong.", tc.in, got, ok, tc.want)
		}
	}
}

func TestParsePercentAgainstDockerFormats(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0.00%", 0, true},
		{"12.34%", 12340, true},
		{"99.99%", 99990, true},
		{"100.00%", 100000, true},
		{"1234.56%", 1234560, true}, // >1 core; docker reports up to 100% per CPU
		{"0.001%", 1, true},
		{" 7.50 % ", 7500, true},
		{"", 0, false},
		{"%", 0, false},
		{"  %", 0, false},
		{"--", 0, false},
		{"abc%", 0, false},
		{"1,234%", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parsePercent(tc.in)
			if ok != tc.ok {
				t.Fatalf("parsePercent(%q) ok = %v, want %v. docker prints \"--\" for a container "+
					"it could not measure; turning that into 0%% would report an idle container "+
					"where there was no measurement at all.", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parsePercent(%q) = %d, want %d milli-percent (percent x 1000)",
					tc.in, got, tc.want)
			}
		})
	}
}

func TestParseUsagePairTakesTheUsedSideOnly(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"408KiB / 30.05GiB", 417792, true},
		{"1.5MiB / 2GiB", 1572864, true},
		{"365.1MiB / 31.19GiB", 382835098, true},
		{"0B / 0B", 0, true},
		{"1.5MiB/2GiB", 1572864, true}, // no spaces
		{"1.5MiB", 1572864, true},      // no limit at all
		{"", 0, false},
		{" / 2GiB", 0, false},
		{"-- / --", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseUsagePair(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseUsagePair(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseUsagePair(%q) = %d, want %d. The right-hand side is the HOST's "+
					"memory limit and says nothing about the target; returning it would make "+
					"every container look identical.", tc.in, got, tc.want)
			}
		})
	}
	// Explicitly: the limit must never be what comes back.
	if got, _ := parseUsagePair("408KiB / 30.05GiB"); got == 32265941811 {
		t.Fatal("parseUsagePair returned the limit rather than the usage")
	}
}

// ---------------------------------------------------------------------------
// 5. applyDockerStats
// ---------------------------------------------------------------------------

// docker stats' MemUsage was measured on this engine to be
// `memory.current - inactive_file`, so it still contains the ACTIVE file cache a
// write workload just created. It must never reach rss_bytes.
func TestApplyDockerStatsNeverTouchesRSS(t *testing.T) {
	s := &Sample{Node: "kv-n1"}
	r := parseSections(healthyExecOutput())
	applyMemory(s, r)
	applyCPU(s, r)
	applyTasks(s, r)
	before, ok := s.RSS()
	if !ok {
		t.Fatal("fixture did not produce an rss to protect")
	}

	applyDockerStats(s, DockerStats{
		Container: "kv-n1", Name: "thesis-kv-n1", ID: "abc123",
		CPUPerc: "12.34%", MemUsage: "365.1MiB / 31.19GiB", MemPerc: "1.14%", PIDs: "37",
	})

	after, ok := s.RSS()
	if !ok {
		t.Fatal("applyDockerStats removed an rss reading that the cgroup had supplied")
	}
	if after != before {
		t.Fatalf("rss changed from %d to %d when a docker stats row was folded in. docker's "+
			"MemUsage is memory.current minus inactive_file: it still carries the active page "+
			"cache, so using it as RSS reintroduces exactly the false resource violation the "+
			"format exists to prevent.", before, after)
	}
	if s.Memory.Source != SourceCgroupV2Anon {
		t.Fatalf("memory.source became %q; the cgroup remains the source of rss_bytes",
			s.Memory.Source)
	}
	if s.Memory.DockerUsageBytes == nil {
		t.Fatal("docker_usage_bytes was dropped; it is a real observation and belongs in the " +
			"record, just not under the name rss_bytes")
	}
	if *s.Memory.DockerUsageBytes != 382835098 {
		t.Fatalf("docker_usage_bytes = %d, want 382835098 (365.1MiB)", *s.Memory.DockerUsageBytes)
	}
	if *s.Memory.DockerUsageBytes == *s.Memory.RSSBytes {
		t.Fatal("docker_usage_bytes and rss_bytes are equal; this fixture was built so they differ")
	}
	if s.CPU == nil || s.CPU.DockerMilliPct == nil || *s.CPU.DockerMilliPct != 12340 {
		t.Fatalf("docker_milli_pct = %v, want 12340", s.CPU)
	}
	if s.CPU.MilliPct != nil {
		t.Fatalf("docker's CPUPerc leaked into cpu.milli_pct (%d); they are different estimators "+
			"and cpu.milli_pct is derived from the cgroup counter across two samples",
			*s.CPU.MilliPct)
	}
	// pids.current from the cgroup is the whole-container figure and must win.
	if s.Tasks == nil || s.Tasks.PIDsCurrent == nil || *s.Tasks.PIDsCurrent != 14 {
		t.Fatalf("pids_current = %v; the cgroup reading (14) must not be overwritten by the "+
			"docker stats row (37)", s.Tasks)
	}
}

// The fallback path for a target with no POSIX shell: docker stats is the only
// source, and it still cannot produce an RSS.
func TestApplyDockerStatsAloneLeavesRSSAbsent(t *testing.T) {
	s := &Sample{Node: "kv-n1"}
	applyDockerStats(s, DockerStats{CPUPerc: "3.21%", MemUsage: "48.5MiB / 31.19GiB", PIDs: "9"})

	if rss, ok := s.RSS(); ok {
		t.Fatalf("rss_bytes = %d from a docker stats row alone. docker stats cannot produce a "+
			"page-cache-free figure at all, so a target with no shell must have rss ABSENT and "+
			"its resource oracle must return INCONCLUSIVE.", rss)
	}
	if s.Memory == nil || s.Memory.DockerUsageBytes == nil {
		t.Fatal("docker_usage_bytes was not recorded")
	}
	if s.Memory.Source != SourceNone {
		t.Fatalf("memory.source = %q with no cgroup reading; it must stay %q",
			s.Memory.Source, SourceNone)
	}
	if s.Memory.CurrentBytes != nil {
		t.Fatalf("current_bytes = %d was invented from docker's MemUsage; they are different "+
			"quantities (MemUsage is memory.current minus inactive_file)", *s.Memory.CurrentBytes)
	}
	if got := observedMetrics(s); containsString(got, MetricMemoryRSS) {
		t.Fatalf("observedMetrics = %v claims memory.rss_bytes was observed. A MemoryMetrics that "+
			"carries only DockerUsageBytes has NOT observed rss, and saying otherwise lets a "+
			"resource oracle believe it has a baseline.", got)
	}
	if !containsString(observedMetrics(s), MetricMemory) {
		t.Fatal("the memory group itself should count as observed once docker supplied a figure")
	}

	// docker prints "--" for a container it could not measure. Nothing may be
	// invented from it.
	empty := &Sample{Node: "kv-n2"}
	applyDockerStats(empty, DockerStats{CPUPerc: "--", MemUsage: "--", PIDs: "--"})
	if empty.Memory != nil {
		t.Fatalf("memory group materialised from an unmeasurable docker row: %+v", empty.Memory)
	}
	if empty.CPU != nil {
		t.Fatalf("cpu group materialised from CPUPerc \"--\": %+v", empty.CPU)
	}
	if empty.Tasks != nil {
		t.Fatalf("tasks group materialised from PIDs \"--\": %+v", empty.Tasks)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func derefI64(p *int64) any {
	if p == nil {
		return "<absent>"
	}
	return *p
}

func mustSection(t *testing.T, r cgroupRead, name string) string {
	t.Helper()
	s, ok := r.section(name)
	if !ok {
		t.Fatalf("section %q missing", name)
	}
	return s
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
