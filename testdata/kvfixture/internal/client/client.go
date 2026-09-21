// Package client is the fixture's HTTP client. It exists so that loadgen,
// provebug and the node binary's own health/steady probes all observe an
// operation the same way, and so that the evidence the ok/fail/info classifier
// needs (was any byte of the request written? did headers arrive? did the body
// carry applied:false?) is gathered rather than guessed.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"prothesis.dev/kvfixture/internal/history"
)

// Client wraps http.Client with request-level evidence collection.
type Client struct {
	hc *http.Client
}

// New builds a client. Keep-alives are on; the per-call context governs
// timeouts so different operation classes can carry different deadlines.
func New(connectTimeout time.Duration, maxConnsPerHost int) *Client {
	if maxConnsPerHost <= 0 {
		maxConnsPerHost = 16
	}
	return &Client{hc: &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   connectTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        maxConnsPerHost * 4,
			MaxIdleConnsPerHost: maxConnsPerHost,
			MaxConnsPerHost:     maxConnsPerHost,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
		},
	}}
}

// CloseIdle releases pooled connections.
func (c *Client) CloseIdle() {
	if tr, ok := c.hc.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

type errBody struct {
	Code       string `json:"code"`
	Applied    *bool  `json:"applied"`
	LeaderHint string `json:"leader_hint"`
	Message    string `json:"message"`
}

// Result is one round trip.
type Result struct {
	Attempt history.Attempt
	Latency time.Duration
	Body    []byte
	Code    string
	// LeaderHint is the server's suggestion of who to talk to instead. It is
	// only ever acted on after a DEFINITE non-execution.
	LeaderHint string
	// StartNS and EndNS are Unix epoch nanoseconds captured immediately around
	// the round trip, so a history record's interval is the true one.
	StartNS int64
	EndNS   int64
}

// Do performs one request. On a 200 with out != nil the body is decoded into
// out. Every outcome is reported through Result.Attempt, which is the sole
// input to history.Classify.
func (c *Client) Do(ctx context.Context, method, url string, body any, out any) Result {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			now := time.Now()
			return Result{
				Attempt: history.Attempt{Err: err},
				StartNS: now.UnixNano(), EndNS: now.UnixNano(),
			}
		}
		buf = bytes.NewReader(b)
	}

	var wrote, gotFirstByte atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
		GotFirstResponseByte: func() { gotFirstByte.Store(true) },
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, url, buf)
	if err != nil {
		now := time.Now()
		return Result{Attempt: history.Attempt{Err: err}, StartNS: now.UnixNano(), EndNS: now.UnixNano()}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	res := Result{StartNS: start.UnixNano()}
	resp, err := c.hc.Do(req)
	if err != nil {
		end := time.Now()
		res.EndNS = end.UnixNano()
		res.Latency = end.Sub(start)
		res.Attempt = history.Attempt{
			WroteRequest: wrote.Load(),
			GotResponse:  gotFirstByte.Load(),
			Err:          err,
		}
		return res
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	end := time.Now()
	res.EndNS = end.UnixNano()
	res.Latency = end.Sub(start)
	res.Body = raw

	att := history.Attempt{
		WroteRequest: true,
		GotResponse:  true,
		Status:       resp.StatusCode,
	}
	if readErr != nil {
		// Headers arrived but the body did not: the operation may well have been
		// applied and the response lost. Indeterminate.
		att.Err = readErr
		res.Attempt = att
		return res
	}

	if resp.StatusCode == http.StatusOK {
		if out != nil {
			if err := json.Unmarshal(raw, out); err == nil {
				att.BodyParsed = true
			}
		} else {
			att.BodyParsed = true
		}
	} else {
		var eb errBody
		if err := json.Unmarshal(raw, &eb); err == nil {
			att.Applied = eb.Applied
			att.Code = eb.Code
			res.Code = eb.Code
			res.LeaderHint = eb.LeaderHint
		}
	}
	res.Attempt = att
	return res
}
