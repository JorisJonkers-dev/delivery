package gate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"time"
)

// The warmup after promotion (deploy-kit spec/v1/55-delivery.md#the-release-gate): once an
// Application serves a new revision, the gate requests the same paths once more through every
// public hostname its members' Canaries hand it, so the request crosses the edge to the new
// primary. A 5xx there fails the release's Rollout and rolls nothing back. The tend loop starts
// it, since Flagger asks no question after promotion.

// Warmup is what the warmup of a serving revision met.
type Warmup struct {
	// Revision is the revision warmed: the one the record says serves.
	Revision string `json:"revision"`
	// Failed says what the first failing request met; empty where none failed.
	Failed string `json:"failed,omitempty"`
}

// warmupTimeout bounds one warmup request, across the edge and a cache that may be cold.
const warmupTimeout = 10 * time.Second

// hostname is a host the gate warms through: a lower-case DNS name of two labels or more. The
// render's schema guarantees it; the gate checks again, and the address it connects to too.
var hostname = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$`) //nolint:gochecknoglobals // a compiled constant

// warm requests every target of every member through each of its hostnames, in member order,
// and stops at the first that fails.
func (g *Gate) warm(ctx context.Context, r release) *Warmup {
	warmup := &Warmup{Revision: r.pinned}
	for _, process := range slices.Sorted(maps.Keys(r.members)) {
		canary := r.members[process]
		targets := targetsOf(canary)
		for _, host := range canary.Hostnames {
			for _, path := range targets {
				if !hostname.MatchString(host) || !requestable.MatchString(path) {
					warmup.Failed = fmt.Sprintf("the Canary of %s hands a host or path the gate does not request", process)
					return warmup
				}
				url := "https://" + host + path
				status, _, err := g.public.Get(ctx, url)
				switch {
				case err != nil:
					warmup.Failed = url + " was not answered"
					return warmup
				case status >= http.StatusInternalServerError:
					warmup.Failed = fmt.Sprintf("%s answered %d", url, status)
					return warmup
				}
			}
		}
	}
	return warmup
}

// PublicTraffic is Traffic across the internet to the edge: `https` on the default port only, no
// proxy, no redirect followed, each hostname resolved as an absolute name and connected to only at
// a public address.
func PublicTraffic() Traffic {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	lookup := func(ctx context.Context, host string) ([]netip.Addr, error) {
		// IPv4 only: the derived internet egress admits no IPv6 address.
		return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's own type
	transport.Proxy = nil
	transport.DialContext = publicDialer(lookup, dialer.DialContext)
	return httpTraffic{client: &http.Client{
		Transport:     transport,
		Timeout:       warmupTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var (
	errNotHTTPS        = errors.New("gate: a warmup connects to port 443 only")
	errNoPublicAddress = errors.New("gate: the hostname resolves to no public address")
)

// publicDialer connects only to a public address on port 443. It resolves the hostname as an
// absolute name, so the cluster's search path never completes it into a Service, and decides on
// each address it would connect to, not on the name: a name that resolves somewhere internal is
// refused whatever it is called.
func publicDialer(
	lookup func(ctx context.Context, host string) ([]netip.Addr, error),
	dial func(ctx context.Context, network, address string) (net.Conn, error),
) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if port != "443" {
			return nil, errNotHTTPS
		}
		addresses, err := lookup(ctx, host+".")
		if err != nil {
			return nil, err
		}
		failed := fmt.Errorf("%w: %s", errNoPublicAddress, host)
		for _, a := range addresses {
			if !public(a) {
				continue
			}
			conn, err := dial(ctx, network, netip.AddrPortFrom(a.Unmap(), 443).String())
			if err == nil {
				return conn, nil
			}
			failed = err
		}
		return nil, failed
	}
}

// notPublic are the ranges that are global unicast to the standard library and reach no public
// host: the carrier-grade NAT range the tailnet uses, and the IETF's special-purpose blocks.
var notPublic = []netip.Prefix{ //nolint:gochecknoglobals // a constant list
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// public reports whether an address is one the warmup may connect to: global unicast, and in no
// private, shared or special-purpose range. Loopback, link-local, multicast and the cluster's own
// pod and service ranges, which are private, are all refused.
func public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
