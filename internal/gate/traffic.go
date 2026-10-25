package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The gate's own traffic on each analysis iteration (deploy-kit
// spec/v1/55-delivery.md#the-release-gate): Flagger switches blue-green without sending the new
// copy a user's request, so the gate requests every path the Canary hands it, at the member's
// Canary service, and holds what it measures to the platform's two thresholds.

// Traffic sends one of the gate's requests.
type Traffic interface {
	// Get requests the URL once, follows no redirect, and reports the status it was answered with
	// and how long the answer took. An error is a request that was not answered.
	Get(ctx context.Context, url string) (status int, took time.Duration, err error)
}

const (
	// samples is how often each path is requested in one iteration, so a p95 is over more than
	// one answer a path.
	samples = 3
	// inFlight bounds how many requests one iteration has open at once.
	inFlight = 4
	// requestTimeout bounds one request: one that takes longer is a timeout, and fails.
	requestTimeout = 2 * time.Second
	// iterationBudget bounds one iteration's requests, inside the 10 s Flagger waits on a webhook.
	iterationBudget = 8 * time.Second
	// maxAnswer is how much of an answer's body is read: enough to time the answer, no more.
	maxAnswer = 1 << 20
)

// HTTPTraffic is Traffic over plain HTTP from inside the cluster: no proxy, no redirect followed,
// a timeout on every request.
func HTTPTraffic() Traffic {
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's own type
	transport.Proxy = nil
	return httpTraffic{client: &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		// A 3xx is an answer, and it passes: the gate never requests its Location.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type httpTraffic struct{ client *http.Client }

func (h httpTraffic) Get(ctx context.Context, url string) (int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	began := time.Now()
	res, err := h.client.Do(req)
	if err != nil {
		return 0, time.Since(began), err
	}
	defer func() { _ = res.Body.Close() }()
	_, err = io.Copy(io.Discard, io.LimitReader(res.Body, maxAnswer))
	return res.StatusCode, time.Since(began), err
}

// requestable is a path the gate builds a request from: URL path segments, each opened by one `/`.
// The render's schema guarantees it; the gate holds API access, so it checks again.
var requestable = regexp.MustCompile(`^/(?:[A-Za-z0-9._~!$&'()*+,;=:@%-]+/?)*$`) //nolint:gochecknoglobals // a compiled constant

// targetsOf is every path the Canary hands the gate, each once: its Route paths, then its
// readiness path.
func targetsOf(c Canary) []string {
	var targets []string
	for _, path := range append(slices.Clone(c.Paths), c.Readiness) {
		if path != "" && !slices.Contains(targets, path) {
			targets = append(targets, path)
		}
	}
	return targets
}

// thresholds reads the platform's two thresholds as the inputs carry them: a percentage and a
// duration.
func thresholds(errorRate, latency string) (float64, time.Duration, error) {
	rate, err := strconv.ParseFloat(strings.TrimSuffix(errorRate, "%"), 64)
	if err != nil || !strings.HasSuffix(errorRate, "%") {
		return 0, 0, fmt.Errorf("gate: the error rate %q is not a percentage", errorRate)
	}
	bound, err := time.ParseDuration(latency)
	if err != nil {
		return 0, 0, fmt.Errorf("gate: the latency %q is not a duration", latency)
	}
	return rate, bound, nil
}

// sample is one request's outcome.
type sample struct {
	path   string
	status int
	took   time.Duration
	err    error
}

func (s sample) failed() bool { return s.err != nil || s.status >= http.StatusInternalServerError }

var errNoPort = errors.New("gate: the Canary names no service port")

// measure sends the iteration's requests to the member's new copy and holds them to the
// thresholds. A request fails on a 5xx or no answer and on nothing else: what the member answers
// a stranger is its own business.
func (g *Gate) measure(ctx context.Context, q Question, read reading, targets []string) (Answer, error) {
	if read.canary.Port <= 0 || read.canary.Port > 65535 {
		return Answer{}, fmt.Errorf("%w: %s", errNoPort, q.Process)
	}
	for _, path := range targets {
		if !requestable.MatchString(path) {
			return Answer{}, fmt.Errorf("gate: the Canary of %s hands a path the gate does not request: %q", q.Process, path)
		}
	}
	maxRate, maxLatency, err := thresholds(read.gate.Analysis.ErrorRate, read.gate.Analysis.Latency)
	if err != nil {
		return Answer{}, err
	}
	base := fmt.Sprintf("http://%s-canary.%s.svc.cluster.local:%d", q.Process, q.Namespace, read.canary.Port)
	results := g.send(ctx, base, targets)

	var failed []sample
	var took []time.Duration
	for _, s := range results {
		if s.failed() {
			failed = append(failed, s)
		}
		if s.err == nil {
			took = append(took, s.took)
		}
	}
	total := len(results)
	if len(failed) == total {
		return no("none of the %d requests to the new copy of %s succeeded; first, %s", total, q.Process, failed[0]), nil
	}
	if rate := float64(len(failed)) * 100 / float64(total); rate > maxRate {
		return no("%d of %d requests to the new copy of %s failed, over the %s the platform allows; first, %s",
			len(failed), total, q.Process, read.gate.Analysis.ErrorRate, failed[0]), nil
	}
	if p95 := percentile95(took); p95 > maxLatency {
		return no("the new copy of %s answered in %s at p95, over the %s the platform allows",
			q.Process, p95.Round(time.Millisecond), read.gate.Analysis.Latency), nil
	}
	return yes(fmt.Sprintf("%d requests, %d failed, %s at p95", total, len(failed), percentile95(took).Round(time.Millisecond))), nil
}

func (s sample) String() string {
	if s.err != nil {
		return s.path + " was not answered"
	}
	return fmt.Sprintf("%s answered %d", s.path, s.status)
}

// send requests every target samples times, at most inFlight at once, within the iteration's
// budget, and returns the outcomes in request order.
func (g *Gate) send(ctx context.Context, base string, targets []string) []sample {
	ctx, cancel := context.WithTimeout(ctx, iterationBudget)
	defer cancel()
	results := make([]sample, 0, len(targets)*samples)
	for range samples {
		for _, path := range targets {
			results = append(results, sample{path: path})
		}
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, inFlight)
	for i := range results {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			s := &results[i]
			s.status, s.took, s.err = g.traffic.Get(ctx, base+s.path)
		})
	}
	wg.Wait()
	return results
}

// percentile95 is the nearest-rank 95th percentile: the smallest duration at least 95% of the
// answers took no longer than.
func percentile95(took []time.Duration) time.Duration {
	sorted := slices.Clone(took)
	slices.Sort(sorted)
	rank := (len(sorted)*95 + 99) / 100
	return sorted[rank-1]
}
