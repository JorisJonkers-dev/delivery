package gate_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// answer is how the fake new copy answers one path.
type answer struct {
	status int
	took   time.Duration
	err    error
}

// copyOf is a fake new copy: what each path answers, and every URL it was asked.
type copyOf struct {
	mu      sync.Mutex
	answers map[string]answer
	asked   []string
}

func (c *copyOf) Get(_ context.Context, url string) (int, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, url)
	path := url[strings.Index(url, ":8080")+len(":8080"):]
	a, held := c.answers[path]
	if !held {
		a = answer{status: http.StatusOK, took: 20 * time.Millisecond}
	}
	return a.status, a.took, a.err
}

// requestable is auth with the API's Canary handing the gate its two Route paths and its readiness path.
func requestable() *cluster {
	c := auth()
	api := c.canaries["auth-api"]
	api.Port, api.Paths, api.Readiness = 8080, []string{"/api", "/api/v1/users/register"}, "/api/actuator/health/readiness"
	c.canaries["auth-api"] = api
	return c
}

var errUnanswered = errors.New("connection refused")

func TestAnIterationHoldsTheGatesOwnRequestsToThePlatformsThresholds(t *testing.T) {
	const healthy = "every pod of the new copy is ready and none restarted, and "
	slow := answer{status: http.StatusOK, took: 700 * time.Millisecond}
	cases := map[string]struct {
		answers map[string]answer
		yes     bool
		why     string
	}{
		"every request answered quickly": {nil, true, healthy + "9 requests, 0 failed, 20ms at p95"},
		// What the member answers a stranger is its own business: only a 5xx or no answer fails.
		"a 401, a 404 and a redirect": {
			map[string]answer{"/api": {status: http.StatusUnauthorized, took: time.Millisecond}, "/api/v1/users/register": {status: http.StatusNotFound}, "/api/actuator/health/readiness": {status: http.StatusFound}},
			true, healthy + "9 requests, 0 failed, 1ms at p95",
		},
		"one path answers 503": {
			map[string]answer{"/api/v1/users/register": {status: http.StatusServiceUnavailable}},
			false, "3 of 9 requests to the new copy of auth-api failed, over the 1% the platform allows; first, /api/v1/users/register answered 503",
		},
		"one path is not answered": {
			map[string]answer{"/api": {err: errUnanswered}},
			false, "3 of 9 requests to the new copy of auth-api failed, over the 1% the platform allows; first, /api was not answered",
		},
		// No data is no.
		"nothing is answered": {
			map[string]answer{"/api": {err: errUnanswered}, "/api/v1/users/register": {err: context.DeadlineExceeded}, "/api/actuator/health/readiness": {err: errUnanswered}},
			false, "none of the 9 requests to the new copy of auth-api succeeded; first, /api was not answered",
		},
		"one path is slow": {
			map[string]answer{"/api": slow},
			false, "the new copy of auth-api answered in 700ms at p95, over the 500ms the platform allows",
		},
		// A slow 5xx fails on the error rate, which is checked first.
		"one path is slow and failing": {
			map[string]answer{"/api": {status: http.StatusBadGateway, took: time.Second}},
			false, "3 of 9 requests to the new copy of auth-api failed, over the 1% the platform allows; first, /api answered 502",
		},
		"exactly at the latency bound": {
			map[string]answer{"/api": {status: http.StatusOK, took: 500 * time.Millisecond}},
			true, healthy + "9 requests, 0 failed, 500ms at p95",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &copyOf{answers: tc.answers}
			got, err := gate.New(requestable(), gate.WithTraffic(fake)).Checks(t.Context(), asks())
			if err != nil || got.Yes != tc.yes || got.Why != tc.why {
				t.Fatalf("checks = %+v, %v", got, err)
			}
			if len(fake.asked) != 9 {
				t.Fatalf("asked %d times: %v", len(fake.asked), fake.asked)
			}
		})
	}
}

func TestTheGateAsksTheMembersCanaryServiceForEveryPathThreeTimes(t *testing.T) {
	fake := &copyOf{}
	c := requestable()
	// The readiness path is also a Route path: it is asked as one path.
	api := c.canaries["auth-api"]
	api.Readiness = "/api"
	c.canaries["auth-api"] = api
	if got, err := gate.New(c, gate.WithTraffic(fake)).Checks(t.Context(), asks()); err != nil || !got.Yes {
		t.Fatalf("checks = %+v, %v", got, err)
	}
	counts := map[string]int{}
	for _, url := range fake.asked {
		counts[url]++
	}
	const service = "http://auth-api-canary.auth-system.svc.cluster.local:8080"
	if len(counts) != 2 || counts[service+"/api"] != 3 || counts[service+"/api/v1/users/register"] != 3 {
		t.Fatalf("asked %v", counts)
	}
}

func TestTheGateSendsNothingItShouldNot(t *testing.T) {
	t.Run("a member with no path to request is held to its pods", func(t *testing.T) {
		fake := &copyOf{}
		got, err := gate.New(auth(), gate.WithTraffic(fake)).Checks(t.Context(), asks())
		if err != nil || !got.Yes || got.Why != "every pod of the new copy is ready and none restarted" || len(fake.asked) != 0 {
			t.Fatalf("checks = %+v, %v, asked %v", got, err, fake.asked)
		}
	})
	t.Run("a new copy whose pods are not ready is not requested", func(t *testing.T) {
		fake := &copyOf{}
		c := requestable()
		c.pods["auth-api"] = []gate.Pod{{Name: "auth-api-7c9d-x"}}
		got, err := gate.New(c, gate.WithTraffic(fake)).Checks(t.Context(), asks())
		if err != nil || got.Yes || len(fake.asked) != 0 {
			t.Fatalf("checks = %+v, %v, asked %v", got, err, fake.asked)
		}
	})
	refused := map[string]func(*gate.Canary){
		"a path that names a host":  func(c *gate.Canary) { c.Paths = []string{"//evil.example/x"} },
		"a path with a query":       func(c *gate.Canary) { c.Paths = []string{"/api?x=1"} },
		"a path that is not one":    func(c *gate.Canary) { c.Paths = []string{"api"} },
		"a readiness path with a #": func(c *gate.Canary) { c.Readiness = "/ready#x" },
		"no service port":           func(c *gate.Canary) { c.Port = 0 },
		"a port out of range":       func(c *gate.Canary) { c.Port = 70000 },
	}
	for name, change := range refused {
		t.Run(name+" is a question the gate cannot answer", func(t *testing.T) {
			fake := &copyOf{}
			c := requestable()
			api := c.canaries["auth-api"]
			change(&api)
			c.canaries["auth-api"] = api
			got, err := gate.New(c, gate.WithTraffic(fake)).Checks(t.Context(), asks())
			if err == nil || got.Yes || len(fake.asked) != 0 {
				t.Fatalf("checks = %+v, %v, asked %v", got, err, fake.asked)
			}
		})
	}
	thresholds := map[string]string{
		"an error rate that is no percentage": `"errorRate":"1%"` + "\x00" + `"errorRate":"1"`,
		"a latency that is no duration":       `"latency":"500ms"` + "\x00" + `"latency":"soon"`,
	}
	for name, edit := range thresholds {
		t.Run(name+" is a question the gate cannot answer", func(t *testing.T) {
			from, to, _ := strings.Cut(edit, "\x00")
			c := requestable()
			c.inputs = strings.Replace(c.inputs, from, to, 1)
			fake := &copyOf{}
			if got, err := gate.New(c, gate.WithTraffic(fake)).Checks(t.Context(), asks()); err == nil || got.Yes || len(fake.asked) != 0 {
				t.Fatalf("checks = %+v, %v", got, err)
			}
		})
	}
}

func TestHTTPTrafficFollowsNoRedirectAndReportsWhatItWasAnswered(t *testing.T) {
	var followed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) })
	mux.HandleFunc("/elsewhere", func(http.ResponseWriter, *http.Request) { followed.Store(true) })
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	traffic := gate.HTTPTraffic()

	if status, took, err := traffic.Get(t.Context(), srv.URL+"/moved"); err != nil || status != http.StatusFound || took <= 0 || followed.Load() {
		t.Fatalf("a redirect = %d, %s, %v, followed %v", status, took, err, followed.Load())
	}
	if status, _, err := traffic.Get(t.Context(), srv.URL+"/broken"); err != nil || status != http.StatusInternalServerError {
		t.Fatalf("a 500 = %d, %v", status, err)
	}
	srv.Close()
	if _, _, err := traffic.Get(t.Context(), srv.URL+"/moved"); err == nil {
		t.Fatal("a copy that is gone answered")
	}
	if _, _, err := traffic.Get(t.Context(), "http://a b/"); err == nil {
		t.Fatal("a URL that does not parse was requested")
	}
}
