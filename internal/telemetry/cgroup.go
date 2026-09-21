package telemetry

import (
	"fmt"
	"strconv"
	"strings"
)

// The batched read the collector performs inside a container.
//
// One `docker exec` per node per interval, measured at 106 ms on the build
// machine against an alpine target. Reading the five files with five separate
// execs would cost ~530 ms, which exceeds the 500 ms default interval on its
// own; hence the shell loop and the delimiter.
//
// A POSIX shell is required. A distroless target has none, the exec fails, and
// every cgroup-derived metric is recorded ABSENT with that reason, never zero.
// See OPEN_QUESTIONS.md OQ-063 for the helper-container route that would lift
// this restriction.
const (
	// readDelimiter prefixes each file's contents in the exec output. `cat a b`
	// concatenates with no separator, so without this there is no way to tell
	// where memory.stat ends and cpu.stat begins.
	readDelimiter = "@@THESIS "

	// pathMemoryStatV2 and friends are cgroup v2 (unified) paths, which is what
	// Docker Desktop's Linux VM presents: verified on the build machine.
	pathMemoryStatV2  = "/sys/fs/cgroup/memory.stat"
	pathMemoryCurrent = "/sys/fs/cgroup/memory.current"
	pathMemorySwap    = "/sys/fs/cgroup/memory.swap.current"
	pathCPUStat       = "/sys/fs/cgroup/cpu.stat"
	pathPidsCurrent   = "/sys/fs/cgroup/pids.current"

	// pathMemoryStatV1 is the cgroup v1 fallback. Its `rss` field is the v1
	// equivalent of v2's `anon`: likewise page-cache-free.
	pathMemoryStatV1 = "/sys/fs/cgroup/memory/memory.stat"

	pathProcStatus = "/proc/1/status"

	// sectionFDCount is a synthetic section: the fd count is produced by a
	// command rather than read from a file.
	sectionFDCount = "fd_count"
)

// readScript is the shell program the collector execs. It never fails the whole
// read because one file is missing: each `cat` falls back to an empty section,
// so a cgroup v1 host still yields cpu.stat and /proc/1/status.
//
// `2>/dev/null` on every read is deliberate. A missing file must produce an
// ABSENT metric, not a stderr blob that the caller might mistake for a
// collection failure and then retry on every interval.
var readScript = strings.Join([]string{
	`for f in ` + pathMemoryStatV2 + ` ` + pathMemoryCurrent + ` ` + pathMemorySwap +
		` ` + pathCPUStat + ` ` + pathPidsCurrent + ` ` + pathMemoryStatV1 + ` ` + pathProcStatus + `; do`,
	`echo "` + readDelimiter + `$f";`,
	`cat "$f" 2>/dev/null;`,
	`done;`,
	`echo "` + readDelimiter + sectionFDCount + `";`,
	`ls /proc/1/fd 2>/dev/null | wc -l`,
}, " ")

// cgroupRead is the parsed result of one batched read.
type cgroupRead struct {
	sections map[string]string
}

// parseSections splits the exec output on the delimiter.
func parseSections(out string) cgroupRead {
	r := cgroupRead{sections: map[string]string{}}
	name := ""
	var b strings.Builder
	flush := func() {
		if name != "" {
			r.sections[name] = b.String()
		}
		b.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if rest, ok := strings.CutPrefix(line, readDelimiter); ok {
			flush()
			name = strings.TrimSpace(rest)
			continue
		}
		if name == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return r
}

// section returns a section's contents and whether it was non-empty. An empty
// section means the file did not exist or could not be read, which is a
// different fact from "the file said 0".
func (r cgroupRead) section(name string) (string, bool) {
	s, ok := r.sections[name]
	if !ok {
		return "", false
	}
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// keyValues parses the `key value` line format shared by memory.stat and
// cpu.stat. Unparseable lines are skipped rather than failing the read: a
// kernel that adds a field with a different shape must not blind the collector
// to every other field.
func keyValues(s string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			continue
		}
		k := line[:i]
		v, err := strconv.ParseInt(strings.TrimSpace(line[i+1:]), 10, 64)
		if err != nil {
			continue
		}
		out[k] = v
	}
	return out
}

// scalar parses a single-number file such as memory.current.
func scalar(s string) (int64, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// procStatusKB parses `VmRSS:  768 kB` into bytes.
func procStatusKB(status, key string) (int64, bool) {
	for _, line := range strings.Split(status, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), key+":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			return v * 1024, true
		}
		return v, true
	}
	return 0, false
}

// procStatusInt parses a bare integer field of /proc/PID/status, such as
// `Threads: 1`.
func procStatusInt(status, key string) (int64, bool) {
	for _, line := range strings.Split(status, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), key+":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// applyMemory fills s.Memory from a batched read.
//
// RSSBytes is set from cgroup v2 `anon`, or failing that from cgroup v1 `rss`,
// and from NOTHING ELSE. When neither is present the metric is marked absent
// with a specific reason: substituting memory.current, docker stats' MemUsage or
// the target's self-report would each silently change what the number means.
func applyMemory(s *Sample, r cgroupRead) {
	m := &MemoryMetrics{Source: SourceNone}

	var haveAnon bool
	if sec, ok := r.section(pathMemoryStatV2); ok {
		kv := keyValues(sec)
		if v, ok := kv["anon"]; ok {
			m.RSSBytes, m.Source, haveAnon = i64(v), SourceCgroupV2Anon, true
		}
		if v, ok := kv["file"]; ok {
			m.PageCacheBytes = i64(v)
		}
		if v, ok := kv["shmem"]; ok {
			m.ShmemBytes = i64(v)
		}
		if v, ok := kv["kernel"]; ok {
			m.KernelBytes = i64(v)
		}
	}
	if !haveAnon {
		if sec, ok := r.section(pathMemoryStatV1); ok {
			kv := keyValues(sec)
			if v, ok := kv["rss"]; ok {
				m.RSSBytes, m.Source, haveAnon = i64(v), SourceCgroupV1RSS, true
			}
			if v, ok := kv["cache"]; ok && m.PageCacheBytes == nil {
				m.PageCacheBytes = i64(v)
			}
		}
	}

	if sec, ok := r.section(pathMemoryCurrent); ok {
		if v, ok := scalar(sec); ok {
			m.CurrentBytes = i64(v)
		}
	}
	if sec, ok := r.section(pathMemorySwap); ok {
		if v, ok := scalar(sec); ok {
			m.SwapBytes = i64(v)
		}
	}
	if sec, ok := r.section(pathProcStatus); ok {
		if v, ok := procStatusKB(sec, "VmRSS"); ok {
			m.PID1RSSBytes = i64(v)
		}
	}

	if !haveAnon {
		s.MarkAbsent(MetricMemoryRSS,
			"neither cgroup v2 %s `anon` nor cgroup v1 %s `rss` was readable; "+
				"memory.current and docker stats MemUsage both include page cache and are NOT substituted",
			pathMemoryStatV2, pathMemoryStatV1)
	}
	if m.RSSBytes == nil && m.CurrentBytes == nil && m.PID1RSSBytes == nil {
		s.MarkAbsent(MetricMemory, "no memory file in the target container was readable")
		return
	}
	s.Memory = m
}

// applyCPU fills s.CPU from a batched read. Only the cumulative counters come
// from here; the rate is derived against the previous sample by the collector.
func applyCPU(s *Sample, r cgroupRead) {
	sec, ok := r.section(pathCPUStat)
	if !ok {
		s.MarkAbsent(MetricCPU, "%s was not readable in the target container", pathCPUStat)
		return
	}
	kv := keyValues(sec)
	c := &CPUMetrics{}
	if v, ok := kv["usage_usec"]; ok {
		c.UsageUS = i64(v)
	}
	if v, ok := kv["user_usec"]; ok {
		c.UserUS = i64(v)
	}
	if v, ok := kv["system_usec"]; ok {
		c.SystemUS = i64(v)
	}
	if v, ok := kv["throttled_usec"]; ok {
		c.ThrottledUS = i64(v)
	}
	if v, ok := kv["nr_throttled"]; ok {
		c.NrThrottled = i64(v)
	}
	if c.UsageUS == nil {
		s.MarkAbsent(MetricCPU, "%s carried no usage_usec field", pathCPUStat)
		return
	}
	s.CPU = c
}

// applyTasks fills s.Tasks from a batched read.
func applyTasks(s *Sample, r cgroupRead) {
	t := &TaskMetrics{}
	if sec, ok := r.section(pathPidsCurrent); ok {
		if v, ok := scalar(sec); ok {
			t.PIDsCurrent = i64(v)
		}
	}
	if sec, ok := r.section(pathProcStatus); ok {
		if v, ok := procStatusInt(sec, "Threads"); ok {
			t.Threads = i64(v)
		}
	}
	if sec, ok := r.section(sectionFDCount); ok {
		if v, ok := scalar(sec); ok {
			t.OpenFDs = i64(v)
		}
	}
	if t.OpenFDs == nil {
		s.MarkAbsent(MetricTasksOpenFDs, "/proc/1/fd was not listable in the target container")
	}
	if t.PIDsCurrent == nil && t.Threads == nil && t.OpenFDs == nil {
		s.MarkAbsent(MetricTasks, "no task-count file in the target container was readable")
		return
	}
	s.Tasks = t
}

// deriveCPURate fills the derived CPU fields from the previous sample of the
// same node.
//
// It is deliberately conservative. A counter that went BACKWARDS means the
// container was recreated and the cgroup counter reset; deriving a rate across
// that boundary would report a large negative or absurd positive value, so the
// derived fields are left nil and the reason is recorded. Likewise a
// non-positive interval, which a coarse host clock can produce.
func deriveCPURate(s *Sample, prev *Sample) {
	if s.CPU == nil || s.CPU.UsageUS == nil {
		return
	}
	if prev == nil || prev.CPU == nil || prev.CPU.UsageUS == nil {
		s.MarkAbsent(MetricCPUMilliPct, "a rate needs two samples; this is the first for this node")
		return
	}
	interval := s.RTNS - prev.RTNS
	if interval <= 0 {
		s.MarkAbsent(MetricCPUMilliPct, "monotonic interval since the previous sample was %dns", interval)
		return
	}
	delta := *s.CPU.UsageUS - *prev.CPU.UsageUS
	if delta < 0 {
		s.MarkAbsent(MetricCPUMilliPct,
			"cgroup cpu.stat usage_usec went backwards (%d -> %d): the container was recreated, "+
				"so no rate spans this boundary", *prev.CPU.UsageUS, *s.CPU.UsageUS)
		return
	}
	s.CPU.DeltaUS = i64(delta)
	s.CPU.IntervalNS = i64(interval)
	// milli_pct = (delta_ns / interval_ns) * 100 * 1000, in integer arithmetic.
	// delta is microseconds, so delta*1000 is nanoseconds; * 100000 then divides
	// by interval_ns. At 8 cores fully saturated over one second this is
	// 8e6 * 1e8 = 8e14, comfortably inside int64.
	s.CPU.MilliPct = i64(delta * 1000 * 100000 / interval)
}

// parsePercent parses docker stats' "12.34%" into milli-percent.
func parsePercent(s string) (int64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	// Round half away from zero, then carry as an integer. The float exists only
	// inside this function: docker prints a decimal string and there is no
	// integer form to ask for.
	v := f * 1000
	if v >= 0 {
		v += 0.5
	} else {
		v -= 0.5
	}
	return int64(v), true
}

// byteUnits maps docker stats' human-readable suffixes to multipliers. docker
// prints SI-prefixed binary quantities ("408KiB", "1.5MiB", "30.05GiB"), and
// also plain "0B".
var byteUnits = []struct {
	suffix string
	mult   float64
}{
	{"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3},
	{"B", 1},
}

// parseBytes parses one side of docker stats' "408KiB / 30.05GiB".
func parseBytes(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, u := range byteUnits {
		num, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
		if err != nil {
			return 0, false
		}
		return int64(f*u.mult + 0.5), true
	}
	return 0, false
}

// parseUsagePair parses docker stats' "408KiB / 30.05GiB", returning the used
// side only. The limit is the host's memory and says nothing about the target.
func parseUsagePair(s string) (int64, bool) {
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return parseBytes(s)
}

// applyDockerStats folds a docker stats row into a sample.
//
// It NEVER touches RSSBytes. MemUsage is `memory.current - inactive_file` on
// this engine, which still contains the active file cache, so it lands in
// DockerUsageBytes under its own name. That is the whole reason this function is
// separate from applyMemory.
func applyDockerStats(s *Sample, row DockerStats) {
	if v, ok := parseUsagePair(row.MemUsage); ok {
		if s.Memory == nil {
			s.Memory = &MemoryMetrics{Source: SourceNone}
		}
		s.Memory.DockerUsageBytes = i64(v)
	}
	if v, ok := parsePercent(row.CPUPerc); ok {
		if s.CPU == nil {
			s.CPU = &CPUMetrics{}
		}
		s.CPU.DockerMilliPct = i64(v)
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(row.PIDs), 10, 64); err == nil {
		if s.Tasks == nil {
			s.Tasks = &TaskMetrics{}
		}
		if s.Tasks.PIDsCurrent == nil {
			s.Tasks.PIDsCurrent = i64(v)
		}
	}
}

// describeExecFailure turns an exec error into the reason string recorded
// against every cgroup-derived metric.
func describeExecFailure(err error) string {
	return fmt.Sprintf("reading cgroup and proc files in the container failed: %v "+
		"(the target image may have no POSIX shell; see OQ-063)", err)
}
