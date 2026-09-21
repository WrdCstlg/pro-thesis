package faults

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// BaselineSchema versions the snapshot artifact.
//
// ADDITIVE: the directive names no schema for it. D-026 requires HEAL to
// compare against an observed baseline rather than an assumed-empty table, and
// a baseline that cannot be version-gated cannot be read back by a later
// release.
const BaselineSchema = "prothesis.net_baseline/v1"

// BaselineFile is the bundle-relative path the snapshot is stored at.
const BaselineFile = "perturber/net_baseline.json"

// The capture script.
//
// One sidecar per node, run at BOOT, producing four sections. Sections are
// delimited by markers rather than parsed positionally, because `ip6tables-save`
// legitimately prints nothing on a host with no IPv6 tables and a positional
// parse would then attribute the next section to it.
const captureScript = "echo '#ADDR'\n" +
	"ip -o addr show\n" +
	"echo '#QDISC'\n" +
	"tc qdisc show\n" +
	"echo '#IPT4'\n" +
	"iptables-save\n" +
	"echo '#IPT6'\n" +
	"ip6tables-save 2>/dev/null || true\n" +
	"echo '#END'\n"

// Iface is one interface of a node, as observed inside its network namespace.
type Iface struct {
	// Name is the interface name, e.g. eth0.
	Name string `json:"name"`
	// CIDRs are its assigned prefixes, e.g. 172.23.0.2/16.
	CIDRs []string `json:"cidrs"`
}

// Baseline is one node's network state before any fault existed.
type Baseline struct {
	// NodeID is the logical node id from prothesis.yaml.
	NodeID string `json:"node_id"`
	// ContainerID is the container the snapshot was taken inside.
	ContainerID string `json:"container_id"`
	// CapturedWallUnixNS is when the snapshot was taken.
	CapturedWallUnixNS int64 `json:"captured_wall_unix_ns"`
	// Ifaces is the interface inventory, sorted by name. It is what maps a
	// peer address to the interface that egresses toward it, and it is captured
	// once because interface names and prefixes are stable for a container's
	// lifetime while `docker inspect` addresses are not.
	Ifaces []Iface `json:"ifaces"`
	// Qdiscs is the normalized `tc qdisc show` output. tc carries no ownership
	// tag, so this list IS the definition of "no residual shaping".
	Qdiscs []string `json:"qdiscs"`
	// IPTables and IP6Tables are the normalized `iptables-save` /
	// `ip6tables-save` output. A target may legitimately ship its own rules
	// (an unmodified nginx:alpine container ships six for Docker's embedded DNS
	// resolver) so this is what an untagged rule is judged against.
	IPTables  []string `json:"iptables"`
	IP6Tables []string `json:"ip6tables"`
}

// BaselineSet is the per-run snapshot artifact.
type BaselineSet struct {
	Schema string     `json:"schema"`
	RunID  string     `json:"run_id"`
	Nodes  []Baseline `json:"nodes"`
}

// Node returns the baseline for a node id.
func (s *BaselineSet) Node(id string) (Baseline, bool) {
	if s == nil {
		return Baseline{}, false
	}
	for _, b := range s.Nodes {
		if b.NodeID == id {
			return b, true
		}
	}
	return Baseline{}, false
}

// Encode renders the snapshot as deterministic JSON.
//
// Determinism matters here for a mundane reason: the run bundle's MANIFEST
// content-addresses every file it finds, so a snapshot whose byte form varied
// between two encodings of the same state would make the bundle hash unstable.
// The type graph is structs and string slices only, no maps, and every slice
// is sorted at capture time, so encoding/json is deterministic on it.
func (s *BaselineSet) Encode() ([]byte, error) { return encodeJSON(s) }

// encodeJSON is the one encoder for this package's bundle artifacts.
//
// encoding/json rather than internal/recorder/cjson: cjson is the WORLD file's
// canonical encoder and refuses maps and json.Marshaler implementations by
// design, which is right for a hashed artifact and needless ceremony for a
// diagnostic one. These types are structs and sorted string slices with no maps
// anywhere, so encoding/json is already deterministic over them.
func encodeJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("faults: encode artifact: %w", err)
	}
	return append(data, '\n'), nil
}

// DecodeBaselines parses a snapshot artifact.
func DecodeBaselines(data []byte) (*BaselineSet, error) {
	var s BaselineSet
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("faults: decode baseline: %w", err)
	}
	if s.Schema != BaselineSchema {
		return nil, fmt.Errorf("faults: baseline schema is %q, want %q", s.Schema, BaselineSchema)
	}
	return &s, nil
}

// ---------------------------------------------------------------------------
// capture parsing
// ---------------------------------------------------------------------------

// capture is one parsed sidecar snapshot.
type capture struct {
	Ifaces    []Iface
	Qdiscs    []string
	IPTables  []string
	IP6Tables []string
}

// parseCapture splits and normalizes the capture script's output.
//
// Normalization is not cosmetic. `iptables-save` stamps a wall-clock comment on
// every dump ("# Generated by iptables-save v1.8.10 ... on Mon Sep 7 09:17:15
// 2026") and prints per-chain packet counters ("[0:0]"). Both change between
// any two dumps of an unchanged table, so a raw comparison reports every node
// as dirty and the residual check becomes noise. `tc qdisc show` similarly
// prints a refcnt that varies with how many namespaces reference the device.
func parseCapture(out string) (capture, error) {
	var c capture
	section := ""
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "#ADDR", "#QDISC", "#IPT4", "#IPT6", "#END":
			section = trimmed
			continue
		}
		if trimmed == "" {
			continue
		}
		switch section {
		case "#ADDR":
			if name, cidr, ok := parseIPAddrLine(trimmed); ok {
				c.Ifaces = addCIDR(c.Ifaces, name, cidr)
			}
		case "#QDISC":
			if n := normalizeQdiscLine(trimmed); n != "" {
				c.Qdiscs = append(c.Qdiscs, n)
			}
		case "#IPT4":
			if n, ok := normalizeIPTablesLine(trimmed); ok {
				c.IPTables = append(c.IPTables, n)
			}
		case "#IPT6":
			if n, ok := normalizeIPTablesLine(trimmed); ok {
				c.IP6Tables = append(c.IP6Tables, n)
			}
		case "":
			return capture{}, fmt.Errorf("faults: snapshot output has no section marker; "+
				"the sidecar produced %q", firstLine(out))
		}
	}
	if section != "#END" {
		return capture{}, fmt.Errorf("faults: snapshot output is truncated "+
			"(last section %q, expected to end at #END)", section)
	}
	sort.Slice(c.Ifaces, func(i, j int) bool { return c.Ifaces[i].Name < c.Ifaces[j].Name })
	for i := range c.Ifaces {
		sort.Strings(c.Ifaces[i].CIDRs)
	}
	sort.Strings(c.Qdiscs)
	return c, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func addCIDR(ifs []Iface, name, cidr string) []Iface {
	for i := range ifs {
		if ifs[i].Name == name {
			ifs[i].CIDRs = append(ifs[i].CIDRs, cidr)
			return ifs
		}
	}
	return append(ifs, Iface{Name: name, CIDRs: []string{cidr}})
}

// parseIPAddrLine reads one `ip -o addr show` record:
//
//	2: eth0    inet 172.23.0.2/16 brd 172.23.255.255 scope global eth0\ ...
//
// The `-o` form keeps one address per line, which is why it is used rather than
// the multi-line default: a line-oriented parser cannot mis-associate an
// address with the wrong interface.
func parseIPAddrLine(line string) (iface, cidr string, ok bool) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return "", "", false
	}
	// f[0] is "<index>:", f[1] is the interface name.
	name := f[1]
	if !ifacePattern.MatchString(name) {
		return "", "", false
	}
	for i := 2; i < len(f)-1; i++ {
		if f[i] == "inet" || f[i] == "inet6" {
			if _, _, err := net.ParseCIDR(f[i+1]); err != nil {
				return "", "", false
			}
			return name, f[i+1], true
		}
	}
	return "", "", false
}

// normalizeQdiscLine strips the varying refcnt from a `tc qdisc show` line and
// collapses whitespace. It returns "" for a line that is not a qdisc record.
func normalizeQdiscLine(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 || f[0] != "qdisc" {
		return ""
	}
	out := make([]string, 0, len(f))
	for i := 0; i < len(f); i++ {
		if f[i] == "refcnt" && i+1 < len(f) {
			i++ // drop "refcnt" and its value
			continue
		}
		out = append(out, f[i])
	}
	return strings.Join(out, " ")
}

// normalizeIPTablesLine drops the dump's wall-clock comments and per-chain
// counters. It returns ok=false for a line that carries no state.
func normalizeIPTablesLine(line string) (string, bool) {
	if strings.HasPrefix(line, "#") {
		// "# Generated by iptables-save ... on <date>" and "# Completed on
		// <date>" differ between every pair of dumps.
		return "", false
	}
	if strings.HasPrefix(line, ":") {
		// ":INPUT ACCEPT [0:0]"; the counters move as packets flow.
		if i := strings.LastIndex(line, " ["); i >= 0 && strings.HasSuffix(line, "]") {
			line = line[:i]
		}
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return "", false
	}
	return line, true
}

// ---------------------------------------------------------------------------
// residue
// ---------------------------------------------------------------------------

// Residue is one node's difference from its BOOT baseline.
type Residue struct {
	// NodeID is the node examined.
	NodeID string `json:"node_id"`
	// NotRunning reports that the container was gone at check time. Its network
	// namespace went with it, so no rule or qdisc of ours can have survived:
	// this is CLEAN, and saying so explicitly is better than an error that
	// looks like a leak.
	NotRunning bool `json:"not_running"`
	// Unreachable records why the node could not be inspected at all. An
	// unreachable node is NOT clean: nothing was verified.
	Unreachable string `json:"unreachable,omitempty"`
	// TaggedRules are surviving iptables rules carrying the `thesis:` prefix.
	// These are unambiguously ours and unambiguously a leak.
	TaggedRules []string `json:"tagged_rules,omitempty"`
	// QdiscAdded are qdisc lines present now and absent at BOOT. tc has no
	// ownership tag, so an added qdisc is the only evidence of residual
	// shaping there can be.
	QdiscAdded []string `json:"qdisc_added,omitempty"`
	// QdiscRemoved are qdisc lines present at BOOT and absent now: shaping the
	// target owned that a withdrawal destroyed. It is reported for the same
	// reason as QdiscAdded: the run must leave the system as it found it.
	QdiscRemoved []string `json:"qdisc_removed,omitempty"`
	// RuleDrift is untagged iptables change against the baseline. It is
	// INFORMATIONAL and never fails HEAL: under the requirement to work on
	// unmodified systems, a target is entitled to change its own rules while
	// the run is in progress, and treating that as our leak is the
	// false-failure D-026 was written to prevent.
	RuleDrift []string `json:"rule_drift,omitempty"`
}

// Clean reports whether this node is free of perturber residue.
func (r Residue) Clean() bool {
	return r.Unreachable == "" &&
		len(r.TaggedRules) == 0 &&
		len(r.QdiscAdded) == 0 &&
		len(r.QdiscRemoved) == 0
}

// Summary renders the residue as one diagnostic line.
func (r Residue) Summary() string {
	if r.Unreachable != "" {
		return fmt.Sprintf("%s: NOT VERIFIED (%s)", r.NodeID, r.Unreachable)
	}
	if r.NotRunning {
		return r.NodeID + ": clean (container gone; its network namespace went with it)"
	}
	if r.Clean() {
		return r.NodeID + ": clean"
	}
	var parts []string
	if n := len(r.TaggedRules); n > 0 {
		parts = append(parts, fmt.Sprintf("%d tagged iptables rule(s)", n))
	}
	if n := len(r.QdiscAdded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d qdisc(s) added since BOOT", n))
	}
	if n := len(r.QdiscRemoved); n > 0 {
		parts = append(parts, fmt.Sprintf("%d qdisc(s) missing since BOOT", n))
	}
	return r.NodeID + ": " + strings.Join(parts, ", ")
}

// diffBaseline compares a fresh capture against a baseline.
func diffBaseline(nodeID string, base Baseline, cur capture) Residue {
	res := Residue{NodeID: nodeID}

	for _, l := range cur.IPTables {
		if strings.Contains(l, TagPrefix) {
			res.TaggedRules = append(res.TaggedRules, l)
		}
	}
	for _, l := range cur.IP6Tables {
		if strings.Contains(l, TagPrefix) {
			res.TaggedRules = append(res.TaggedRules, l)
		}
	}

	res.QdiscAdded = subtractMultiset(cur.Qdiscs, base.Qdiscs)
	res.QdiscRemoved = subtractMultiset(base.Qdiscs, cur.Qdiscs)

	for _, l := range subtractMultiset(cur.IPTables, base.IPTables) {
		if !strings.Contains(l, TagPrefix) {
			res.RuleDrift = append(res.RuleDrift, "+ "+l)
		}
	}
	for _, l := range subtractMultiset(base.IPTables, cur.IPTables) {
		if !strings.Contains(l, TagPrefix) {
			res.RuleDrift = append(res.RuleDrift, "- "+l)
		}
	}
	sort.Strings(res.RuleDrift)
	return res
}

// subtractMultiset returns the elements of a not covered by b, counting
// duplicates. Two identical DROP rules are two rules, and a residual check that
// deduplicated would miss the second.
func subtractMultiset(a, b []string) []string {
	remaining := make(map[string]int, len(b))
	for _, s := range b {
		remaining[s]++
	}
	var out []string
	for _, s := range a {
		if remaining[s] > 0 {
			remaining[s]--
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// ResidualReport
// ---------------------------------------------------------------------------

// ResidualReport is what HEAL evaluates.
type ResidualReport struct {
	Schema string    `json:"schema"`
	RunID  string    `json:"run_id"`
	Nodes  []Residue `json:"nodes"`
	// StraySidecars is the number of LIVE sidecar containers a sweep had to
	// remove. Non-zero is a defect: `--rm` plus Sidecar.Exec's error path should
	// have removed every one.
	StraySidecars int `json:"stray_sidecars"`
	// StraySidecarIDs names them. A count alone is not actionable: the whole
	// question when one turns up is WHICH container and whose namespace it was
	// holding open.
	StraySidecarIDs []string `json:"stray_sidecar_ids,omitempty"`
	// CheckedWallUnixNS is when the check ran.
	CheckedWallUnixNS int64 `json:"checked_wall_unix_ns"`
}

// ResidualSchema versions the report. ADDITIVE.
const ResidualSchema = "prothesis.net_residual/v1"

// ResidualFile is the bundle-relative path the report is stored at.
const ResidualFile = "perturber/net_residual.json"

// Clean reports whether every node verified clean and no sidecar leaked.
func (r *ResidualReport) Clean() bool {
	if r == nil {
		return false
	}
	if r.StraySidecars != 0 {
		return false
	}
	for _, n := range r.Nodes {
		if !n.Clean() {
			return false
		}
	}
	return true
}

// Err returns a diagnostic error when the report is not clean, and nil when it
// is.
//
// It is an error and not a warning by construction. A surviving DROP rule or
// netem qdisc silently poisons every world executed afterwards on the same
// topology, and the failure surfaces as an unrelated world failing for reasons
// nobody can reproduce.
func (r *ResidualReport) Err() error {
	if r == nil {
		return fmt.Errorf("faults: residual verification did not run")
	}
	if r.Clean() {
		return nil
	}
	var parts []string
	for _, n := range r.Nodes {
		if !n.Clean() {
			parts = append(parts, n.Summary())
		}
	}
	if r.StraySidecars != 0 {
		parts = append(parts, fmt.Sprintf("%d sidecar container(s) outlived their --rm: %s",
			r.StraySidecars, strings.Join(r.StraySidecarIDs, ", ")))
	}
	return fmt.Errorf("faults: HEAL found residue: %s", strings.Join(parts, "; "))
}

// Encode renders the report as deterministic JSON.
func (r *ResidualReport) Encode() ([]byte, error) { return encodeJSON(r) }

func nowNS() int64 { return time.Now().UnixNano() }
