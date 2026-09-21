package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultHTTPTimeout bounds one probe or status request.
//
// Deliberately short. A node that has not answered in two seconds has already
// told the collector what it needed to know; waiting longer would delay the
// whole sample and blur the very latency signal the request exists to measure.
const DefaultHTTPTimeout = 2 * time.Second

// maxStatusBody bounds how much of a /status response is read.
//
// The target is under active fault injection and may return anything at all,
// including an unbounded stream. 256 KiB is far beyond any plausible status
// document and far below anything that would threaten the harness.
const maxStatusBody = 256 << 10

// fetch performs one GET and reports status, latency and body.
//
// Latency is measured across the whole exchange including reading the body,
// because a target that sends headers promptly and then stalls is not healthy,
// and a measurement that stopped at the header would call it so.
func fetch(ctx context.Context, client *http.Client, url string, limit int64) (code int, latency time.Duration, body []byte, err error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, time.Since(start), nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Since(start), nil, err
	}
	defer resp.Body.Close()
	if limit > 0 {
		body, err = io.ReadAll(io.LimitReader(resp.Body, limit))
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxStatusBody))
	}
	return resp.StatusCode, time.Since(start), body, err
}

// collectProbe runs one health probe and records the result.
//
// A non-2xx answer and a transport error are both recorded with OK=false, and
// both keep their latency: how long a node took to refuse is itself a signal.
// A probe that was not attempted leaves s.Probe nil, which is a different fact
// from a probe that failed.
func collectProbe(ctx context.Context, client *http.Client, s *Sample, url string) {
	if url == "" {
		s.MarkAbsent(MetricProbe, "no health probe is declared for this node")
		return
	}
	code, latency, _, err := fetch(ctx, client, url, 0)
	p := &ProbeMetrics{URL: url, LatencyUS: i64(latency.Microseconds())}
	if err != nil {
		p.Error = err.Error()
		s.Probe = p
		return
	}
	p.StatusCode = i64(int64(code))
	p.OK = code >= 200 && code < 300
	if !p.OK {
		p.Error = fmt.Sprintf("HTTP %d", code)
	}
	s.Probe = p
}

// collectStatus fetches the target's own status document.
//
// The body is kept VERBATIM in Raw, and the well-known fields are extracted
// alongside it. The two are not alternatives: extraction is a convenience over
// a vocabulary this package chose, and Raw is the guarantee that choosing it
// destroyed nothing.
func collectStatus(ctx context.Context, client *http.Client, s *Sample, url string) {
	if url == "" {
		s.MarkAbsent(MetricStatus, "the target declares no status endpoint")
		s.MarkAbsent(MetricStatusQueue,
			"queue depth is not observable from an unmodified process and no status endpoint reports it")
		s.MarkAbsent(MetricStatusRoutine,
			"goroutine count is not observable from an unmodified process and no status endpoint reports it")
		return
	}
	code, latency, body, err := fetch(ctx, client, url, maxStatusBody)
	st := &StatusMetrics{URL: url, LatencyUS: i64(latency.Microseconds())}
	if err != nil {
		st.Error = err.Error()
		s.Status = st
		s.MarkAbsent(MetricStatusQueue, "status endpoint %s did not answer: %v", url, err)
		s.MarkAbsent(MetricStatusRoutine, "status endpoint %s did not answer: %v", url, err)
		return
	}
	st.StatusCode = i64(int64(code))
	st.OK = code >= 200 && code < 300
	if !st.OK {
		st.Error = fmt.Sprintf("HTTP %d", code)
	}
	if st.OK {
		extractStatus(st, body)
	}
	s.Status = st

	if st.QueueDepth == nil {
		s.MarkAbsent(MetricStatusQueue, "status endpoint %s reported no queue_depth", url)
	}
	if st.Goroutines == nil {
		s.MarkAbsent(MetricStatusRoutine, "status endpoint %s reported no goroutines", url)
	}
}

// extractStatus pulls the well-known vocabulary out of a status document.
//
// The vocabulary is ADDITIVE and OPEN: it is the intersection of what the kv
// fixture publishes and what the directive's built-in oracles need. A target
// spelling a field differently loses only the extracted convenience; Raw still
// carries its document intact, so nothing is destroyed and a later phase can
// widen the vocabulary without a format change.
//
// Every field is filled only when the document carried that key WITH A VALUE OF
// THE RIGHT TYPE. A key present as null, or as a string where a number was
// expected, leaves the field nil: absent, not zero.
func extractStatus(st *StatusMetrics, body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return
	}
	st.Raw = append(json.RawMessage(nil), trimmed...)

	st.Role = jsonString(doc, "role")
	st.LeaderID = jsonString(doc, "leader_id")
	st.Term = jsonInt(doc, "term")
	st.CommitIndex = jsonInt(doc, "commit_index")
	st.QueueDepth = jsonInt(doc, "queue_depth")
	st.Goroutines = jsonInt(doc, "goroutines")
	st.ClientConnections = jsonInt(doc, "client_connections")
	st.PeerConnections = jsonInt(doc, "peer_connections")
	st.UptimeMS = jsonInt(doc, "uptime_ms")
	st.ReportedRSSBytes = jsonInt(doc, "rss_bytes")

	if raw, ok := doc["lease"]; ok {
		var lease map[string]json.RawMessage
		if err := json.Unmarshal(raw, &lease); err == nil {
			l := &LeaseStatus{
				Held:        jsonBool(lease, "held"),
				RemainingMS: jsonInt(lease, "remaining_ms"),
				DurationMS:  jsonInt(lease, "duration_ms"),
			}
			if l.Held != nil || l.RemainingMS != nil || l.DurationMS != nil {
				st.Lease = l
			}
		}
	}
}

// jsonInt extracts an integer, refusing anything that is not one.
//
// json.Number rather than float64: a commit index above 2^53 would lose
// precision through float64, and a silently wrong commit index is worse than an
// absent one.
func jsonInt(doc map[string]json.RawMessage, key string) *int64 {
	raw, ok := doc[key]
	if !ok {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	i, err := n.Int64()
	if err != nil {
		return nil
	}
	return i64(i)
}

func jsonString(doc map[string]json.RawMessage, key string) *string {
	raw, ok := doc[key]
	if !ok {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return strp(s)
}

func jsonBool(doc map[string]json.RawMessage, key string) *bool {
	raw, ok := doc[key]
	if !ok {
		return nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil
	}
	return boolp(b)
}
