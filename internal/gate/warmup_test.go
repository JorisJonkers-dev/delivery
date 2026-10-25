package gate_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// edge is a fake public edge: what each URL answers, and every URL it was asked.
type edge struct {
	mu      sync.Mutex
	answers map[string]answer
	asked   []string
}

func (e *edge) Get(_ context.Context, url string) (int, time.Duration, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked = append(e.asked, url)
	a, held := e.answers[url]
	if !held {
		return http.StatusOK, 30 * time.Millisecond, nil
	}
	return a.status, a.took, a.err
}

// promoted is auth just promoted at the revision under release, both members handing the gate
// their paths and auth's public host.
func promoted() *cluster {
	c := auth()
	api := of("Succeeded", "a2", "a2", 4)
	api.Paths, api.Readiness, api.Hostnames = []string{"/api"}, "/ready", []string{"auth.jorisjonkers.dev"}
	ui := of("Initialized", "u1", "u1", 0)
	ui.Paths, ui.Readiness, ui.Hostnames = []string{"/"}, "/", []string{"auth.jorisjonkers.dev"}
	c.canaries = map[string]gate.Canary{"auth-api": api, "auth-ui": ui}
	c.record = &gate.Record{Serving: earlier, Promoted: map[string]string{"auth-api": "a1", "auth-ui": "u1"}, Pinned: revision, Since: first}
	return c
}

func warmed(t *testing.T, c *cluster, e *edge) *gate.Warmup {
	t.Helper()
	if err := gate.New(c, gate.WithPublicTraffic(e)).Tend(t.Context(), now, quiet); err != nil {
		t.Fatal(err)
	}
	if len(c.written) == 0 {
		t.Fatal("no record was written")
	}
	return c.written[len(c.written)-1].Warmup
}

func TestOnceAnApplicationServesANewRevisionTheGateWarmsItThroughEveryPublicHostOnce(t *testing.T) {
	c, e := promoted(), &edge{}

	got := warmed(t, c, e)

	want := []string{"https://auth.jorisjonkers.dev/api", "https://auth.jorisjonkers.dev/ready", "https://auth.jorisjonkers.dev/"}
	if got == nil || *got != (gate.Warmup{Revision: revision}) || strings.Join(e.asked, " ") != strings.Join(want, " ") {
		t.Fatalf("warmup = %+v, asked %v", got, e.asked)
	}
	if c.written[0].Serving != revision {
		t.Fatalf("record = %+v", c.written[0])
	}
	// The revision is warmed: the next round asks nothing and writes nothing.
	if err := gate.New(c, gate.WithPublicTraffic(e)).Tend(t.Context(), now, quiet); err != nil {
		t.Fatal(err)
	}
	if len(e.asked) != len(want) || len(c.written) != 1 {
		t.Fatalf("a second round asked %v and wrote %+v", e.asked, c.written)
	}
}

func TestAWarmupFailsOnTheFirst5xxOrUnansweredRequestAndOnNothingElse(t *testing.T) {
	cases := map[string]struct {
		answers map[string]answer
		failed  string
		asked   int
	}{
		"a 401, a 404 and a redirect pass": {
			map[string]answer{
				"https://auth.jorisjonkers.dev/api":   {status: http.StatusUnauthorized},
				"https://auth.jorisjonkers.dev/ready": {status: http.StatusNotFound},
				"https://auth.jorisjonkers.dev/":      {status: http.StatusFound},
			},
			"", 3,
		},
		"a 502 stops the warmup": {
			map[string]answer{"https://auth.jorisjonkers.dev/api": {status: http.StatusBadGateway}},
			"https://auth.jorisjonkers.dev/api answered 502", 1,
		},
		"no answer stops the warmup": {
			map[string]answer{"https://auth.jorisjonkers.dev/ready": {err: errUnanswered}},
			"https://auth.jorisjonkers.dev/ready was not answered", 2,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, e := promoted(), &edge{answers: tc.answers}
			got := warmed(t, c, e)
			if got == nil || got.Revision != revision || got.Failed != tc.failed || len(e.asked) != tc.asked {
				t.Fatalf("warmup = %+v, asked %v", got, e.asked)
			}
		})
	}
}

func TestTheGateWarmsNothingItShouldNot(t *testing.T) {
	t.Run("an Application no public host reaches is warmed by asking nothing", func(t *testing.T) {
		c, e := auth(), &edge{}
		c.canaries = map[string]gate.Canary{"auth-api": of("Succeeded", "a2", "a2", 4), "auth-ui": of("Initialized", "u1", "u1", 0)}
		c.record = &gate.Record{Serving: earlier, Pinned: revision, Since: first}
		if got := warmed(t, c, e); got == nil || *got != (gate.Warmup{Revision: revision}) || len(e.asked) != 0 {
			t.Fatalf("warmup = %+v, asked %v", got, e.asked)
		}
	})
	t.Run("a release still under way is not warmed", func(t *testing.T) {
		c, e := promoted(), &edge{}
		api := c.canaries["auth-api"]
		api.Phase = "Progressing"
		c.canaries["auth-api"] = api
		if err := gate.New(c, gate.WithPublicTraffic(e)).Tend(t.Context(), now, quiet); err != nil {
			t.Fatal(err)
		}
		if len(e.asked) != 0 {
			t.Fatalf("asked %v", e.asked)
		}
	})
	for name, change := range map[string]func(*gate.Canary){
		"a host that is not lower case": func(c *gate.Canary) { c.Hostnames = []string{"Auth.jorisjonkers.dev"} },
		"a host with one label":         func(c *gate.Canary) { c.Hostnames = []string{"auth"} },
		"a host with a port":            func(c *gate.Canary) { c.Hostnames = []string{"auth.jorisjonkers.dev:8443"} },
		"a path with a query":           func(c *gate.Canary) { c.Paths = []string{"/api?x"} },
	} {
		t.Run(name+" fails the warmup and is not requested", func(t *testing.T) {
			c, e := promoted(), &edge{}
			api := c.canaries["auth-api"]
			change(&api)
			c.canaries["auth-api"] = api
			got := warmed(t, c, e)
			if got == nil || got.Failed != "the Canary of auth-api hands a host or path the gate does not request" || len(e.asked) != 0 {
				t.Fatalf("warmup = %+v, asked %v", got, e.asked)
			}
		})
	}
}

func TestTheReleasesSayWhatTheWarmupMet(t *testing.T) {
	c := promoted()
	c.record = &gate.Record{Serving: revision, Warmup: &gate.Warmup{Revision: revision, Failed: "https://auth.jorisjonkers.dev/ answered 503"}}
	releases, err := gate.New(c).Releases(t.Context())
	if err != nil || len(releases.Releases) != 1 {
		t.Fatalf("releases = %+v, %v", releases, err)
	}
	if w := releases.Releases[0].Warmup; w == nil || *w != *c.record.Warmup {
		t.Fatalf("warmup = %+v", w)
	}
}

var errLookup = errors.New("no such host")

func TestAWarmupConnectsOnlyToAPublicAddressOnPort443(t *testing.T) {
	var looked []string
	addresses := map[string][]string{
		"public.example.":   {"10.42.0.7", "127.0.0.1", "203.0.113.9"},
		"internal.example.": {"10.43.0.10", "169.254.1.1", "100.100.1.1", "::1", "fd00::1"},
		"down.example.":     {"203.0.113.10"},
	}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		looked = append(looked, host)
		listed, held := addresses[host]
		if !held {
			return nil, errLookup
		}
		out := make([]netip.Addr, 0, len(listed))
		for _, a := range listed {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	var dialed []string
	errDown := errors.New("connection refused")
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		if address == "203.0.113.10:443" {
			return nil, errDown
		}
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		return client, nil
	}
	connect := gate.PublicDialer(lookup, dial)

	if conn, err := connect(t.Context(), "tcp", "public.example:443"); err != nil || conn == nil {
		t.Fatalf("a public host = %v, %v", conn, err)
	}
	// Resolved as an absolute name, and connected to at its one public address.
	if looked[0] != "public.example." || strings.Join(dialed, " ") != "203.0.113.9:443" {
		t.Fatalf("looked up %v, dialed %v", looked, dialed)
	}
	refused := map[string]error{
		"internal.example:443": gate.ErrNoPublicAddress,
		"public.example:80":    gate.ErrNotHTTPS,
		"missing.example:443":  errLookup,
		"down.example:443":     errDown,
	}
	for address, want := range refused {
		if conn, err := connect(t.Context(), "tcp", address); conn != nil || !errors.Is(err, want) {
			t.Fatalf("%s = %v, %v, want %v", address, conn, err, want)
		}
	}
	if _, err := connect(t.Context(), "tcp", "no port"); err == nil {
		t.Fatal("an address with no port was dialed")
	}
	if strings.Join(dialed, " ") != "203.0.113.9:443 203.0.113.10:443" {
		t.Fatalf("dialed %v", dialed)
	}
}

func TestOnlyAPublicAddressIsPublic(t *testing.T) {
	for address, want := range map[string]bool{
		"203.0.113.9": true, "2606:4700::1": true, "::ffff:203.0.113.9": true,
		"10.42.0.7": false, "172.16.0.1": false, "192.168.1.1": false, "127.0.0.1": false,
		"169.254.1.1": false, "100.64.0.1": false, "100.127.255.254": false, "192.0.0.1": false,
		"198.18.0.1": false, "0.0.0.0": false, "224.0.0.1": false, "::1": false, "fe80::1": false,
		"fd00::1": false, "2001:db8::1": false, "::ffff:10.0.0.1": false,
	} {
		if got := gate.Public(netip.MustParseAddr(address)); got != want {
			t.Errorf("%s: public = %v", address, got)
		}
	}
}

func TestPublicTrafficRefusesAnythingButHTTPSToAPublicAddress(t *testing.T) {
	traffic := gate.PublicTraffic()
	for _, url := range []string{"https://localhost/", "http://203.0.113.9/"} {
		if _, _, err := traffic.Get(t.Context(), url); err == nil {
			t.Fatalf("%s was requested", url)
		}
	}
}
