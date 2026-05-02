// Package monitor performs concurrent HTTP health checks against a list of
// URLs and records latency / outcome metrics through OpenTelemetry.
package monitor

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/wayne-ngo/cloudpulse/internal/metrics"
)

const (
	// StatusUp is reported when the target responded with a 2xx or 3xx code.
	StatusUp = "up"
	// StatusDown is reported on transport errors, timeouts, or any 4xx/5xx.
	StatusDown = "down"

	// DefaultTimeout is the per-request timeout used when callers don't
	// supply their own http.Client.
	DefaultTimeout = 5 * time.Second
)

// CheckResult is the per-URL outcome returned to API callers and serialized
// directly to JSON.
type CheckResult struct {
	URL        string `json:"url"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMs  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// Pinger executes concurrent health checks. It is safe for concurrent use
// across goroutines because the embedded http.Client is goroutine-safe and
// the OTel instruments handle their own synchronization.
type Pinger struct {
	client      *http.Client
	logger      *zap.Logger
	instruments *metrics.Instruments
}

// New constructs a Pinger. Passing a nil client falls back to a client with
// DefaultTimeout. Logger and instruments may also be nil for unit tests; in
// that case logging and metric recording are skipped.
func New(client *http.Client, logger *zap.Logger, instruments *metrics.Instruments) *Pinger {
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	return &Pinger{
		client:      client,
		logger:      logger,
		instruments: instruments,
	}
}

// CheckURLs fans out one goroutine per URL, pings each, and returns the
// per-URL results in the same order as the input slice. The returned slice
// always has len(urls) entries; failures are reported as StatusDown rather
// than returned as a Go error.
func (p *Pinger) CheckURLs(ctx context.Context, urls []string) []CheckResult {
	results := make([]CheckResult, len(urls))
	if len(urls) == 0 {
		return results
	}

	var wg sync.WaitGroup
	wg.Add(len(urls))

	for i, url := range urls {
		// Each goroutine writes to its own pre-allocated index, so no mutex
		// is required when collecting results.
		go func(idx int, target string) {
			defer wg.Done()
			results[idx] = p.checkOne(ctx, target)
		}(i, url)
	}

	wg.Wait()
	return results
}

// checkOne performs a single GET request, records metrics, and returns the
// outcome. It is the unit that goroutines invoke from CheckURLs.
func (p *Pinger) checkOne(ctx context.Context, url string) CheckResult {
	result := CheckResult{URL: url, Status: StatusDown}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		result.Error = err.Error()
		p.recordMetrics(ctx, result.Status, 0)
		p.logCheck(url, result, err)
		return result
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	elapsed := time.Since(start)
	result.LatencyMs = elapsed.Milliseconds()

	if err != nil {
		result.Error = classifyError(err)
		p.recordMetrics(ctx, result.Status, elapsed)
		p.logCheck(url, result, err)
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		result.Status = StatusUp
	}

	p.recordMetrics(ctx, result.Status, elapsed)
	p.logCheck(url, result, nil)
	return result
}

func (p *Pinger) recordMetrics(ctx context.Context, status string, elapsed time.Duration) {
	if p.instruments == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("status", status))
	p.instruments.PingsTotal.Add(ctx, 1, attrs)
	// Record even on zero-elapsed (request-build failures) so the histogram
	// reflects all attempts.
	p.instruments.LatencyHistogram.Record(ctx, float64(elapsed.Milliseconds()), attrs)
}

func (p *Pinger) logCheck(url string, r CheckResult, err error) {
	if p.logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("url", url),
		zap.String("status", r.Status),
		zap.Int("status_code", r.StatusCode),
		zap.Int64("latency_ms", r.LatencyMs),
	}
	if err != nil {
		p.logger.Warn("ping failed", append(fields, zap.Error(err))...)
		return
	}
	p.logger.Info("ping completed", fields...)
}

// classifyError produces a stable, human-readable error string. Net/http's
// raw error messages embed addresses and timestamps that change between
// runs, which is a pain in tests and dashboards.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	return err.Error()
}
