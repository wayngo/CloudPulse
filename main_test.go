package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wayne-ngo/cloudpulse/internal/monitor"
)

// newPinger builds a Pinger configured for fast tests: short timeout and no
// logger / metrics wired up, so failures don't pollute test output.
func newPinger(timeout time.Duration) *monitor.Pinger {
	return monitor.New(&http.Client{Timeout: timeout}, nil, nil)
}

func TestCheckURLs_AllUp(t *testing.T) {
	servers := make([]*httptest.Server, 3)
	urls := make([]string, 3)
	for i := range servers {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(s.Close)
		servers[i] = s
		urls[i] = s.URL
	}

	results := newPinger(2 * time.Second).CheckURLs(context.Background(), urls)
	if len(results) != len(urls) {
		t.Fatalf("expected %d results, got %d", len(urls), len(results))
	}
	for i, r := range results {
		if r.URL != urls[i] {
			t.Errorf("result %d url mismatch: got %q want %q", i, r.URL, urls[i])
		}
		if r.Status != monitor.StatusUp {
			t.Errorf("result %d expected status up, got %q (err=%q)", i, r.Status, r.Error)
		}
		if r.StatusCode != http.StatusOK {
			t.Errorf("result %d expected 200, got %d", i, r.StatusCode)
		}
		if r.LatencyMs < 0 {
			t.Errorf("result %d negative latency: %d", i, r.LatencyMs)
		}
	}
}

func TestCheckURLs_MixedStatuses(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(okSrv.Close)

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(errSrv.Close)

	// http://127.0.0.1:1 should always refuse / fail to connect.
	deadURL := "http://127.0.0.1:1"

	urls := []string{okSrv.URL, errSrv.URL, deadURL}
	results := newPinger(2 * time.Second).CheckURLs(context.Background(), urls)

	if results[0].Status != monitor.StatusUp || results[0].StatusCode != 200 {
		t.Errorf("expected ok server up/200, got %+v", results[0])
	}
	if results[1].Status != monitor.StatusDown || results[1].StatusCode != 500 {
		t.Errorf("expected 500 server down/500, got %+v", results[1])
	}
	if results[2].Status != monitor.StatusDown {
		t.Errorf("expected dead URL down, got %+v", results[2])
	}
	if results[2].Error == "" {
		t.Errorf("expected dead URL to carry an error message, got empty")
	}
}

func TestCheckURLs_Timeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	results := newPinger(50 * time.Millisecond).CheckURLs(context.Background(), []string{slow.URL})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Status != monitor.StatusDown {
		t.Errorf("expected status down on timeout, got %q", r.Status)
	}
	if r.Error == "" {
		t.Errorf("expected error message on timeout")
	}
	// Sanity check: the recorded latency should be at least the client
	// timeout but well below the server's sleep.
	if r.LatencyMs < 40 || r.LatencyMs > 400 {
		t.Errorf("unexpected latency on timeout: %dms", r.LatencyMs)
	}
}

func TestCheckURLs_Concurrency(t *testing.T) {
	const (
		count = 10
		delay = 100 * time.Millisecond
	)

	var inFlight int32
	var maxInFlight int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Track maximum simultaneous in-flight requests as a direct proof
		// of fan-out, independent of wall-clock noise on slow CI runners.
		current := atomic.AddInt32(&inFlight, 1)
		for {
			prev := atomic.LoadInt32(&maxInFlight)
			if current <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, current) {
				break
			}
		}
		time.Sleep(delay)
		atomic.AddInt32(&inFlight, -1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	urls := make([]string, count)
	for i := range urls {
		urls[i] = srv.URL
	}

	start := time.Now()
	results := newPinger(2 * time.Second).CheckURLs(context.Background(), urls)
	elapsed := time.Since(start)

	if len(results) != count {
		t.Fatalf("expected %d results, got %d", count, len(results))
	}
	for i, r := range results {
		if r.Status != monitor.StatusUp {
			t.Errorf("result %d not up: %+v", i, r)
		}
	}

	// If pings ran sequentially the wall clock would be ~count*delay = 1s.
	// A generous 600ms ceiling avoids flakes on contended CI runners while
	// still failing if concurrency regressed to serial execution.
	if elapsed > 600*time.Millisecond {
		t.Errorf("checks took %v, expected concurrent execution under 600ms", elapsed)
	}

	if got := atomic.LoadInt32(&maxInFlight); got < 2 {
		t.Errorf("expected >= 2 concurrent in-flight requests, observed %d", got)
	}
}

func TestCheckURLs_EmptyInput(t *testing.T) {
	results := newPinger(time.Second).CheckURLs(context.Background(), nil)
	if len(results) != 0 {
		t.Errorf("expected empty results for nil input, got %d", len(results))
	}

	results = newPinger(time.Second).CheckURLs(context.Background(), []string{})
	if len(results) != 0 {
		t.Errorf("expected empty results for empty input, got %d", len(results))
	}
}

func TestCheckURLs_InvalidURL(t *testing.T) {
	// A URL containing a control character fails NewRequestWithContext
	// before any network call, exercising the request-build error path.
	results := newPinger(time.Second).CheckURLs(context.Background(), []string{"http://exa\x7fmple.com"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Status != monitor.StatusDown {
		t.Errorf("expected status down for invalid URL, got %q", r.Status)
	}
	if !strings.Contains(strings.ToLower(r.Error), "invalid") && r.Error == "" {
		// We don't assert the exact net/http message, but it must not be empty.
		t.Errorf("expected non-empty error for invalid URL, got %q", r.Error)
	}
}
