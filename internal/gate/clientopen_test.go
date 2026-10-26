package gate_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// nativeInputs is auth's inputs with a native Client whose protected path the API serves.
var nativeInputs = strings.Replace(authInputs, `}]}`, `}],"protectedBy":"auth-api","protectedPath":"/api/me"}`, 1) //nolint:gochecknoglobals // a fixture

// signingIn is auth at its barrier with a native Client: the API analysed and asking, the UI
// waiting, both Canaries naming their service port.
func signingIn() *cluster {
	c := auth()
	c.inputs = nativeInputs
	api := c.canaries["auth-api"]
	api.Port = 8080
	ui := of("WaitingPromotion", "u2", "u1", 0)
	ui.Port = 8080
	c.canaries["auth-api"], c.canaries["auth-ui"] = api, ui
	return c
}

func TestAMemberIsPromotedOnlyOnceTheProtectedPathIsRefusedToAStranger(t *testing.T) {
	const passed = "every member passed its analysis or did not change"
	cases := map[string]struct {
		answer answer
		yes    bool
		why    string
	}{
		"a 401":      {answer{status: http.StatusUnauthorized}, true, passed + ", and auth-api refuses /api/me to a stranger (401)"},
		"a redirect": {answer{status: http.StatusFound}, true, passed + ", and auth-api refuses /api/me to a stranger (302)"},
		"a 404":      {answer{status: http.StatusNotFound}, true, passed + ", and auth-api refuses /api/me to a stranger (404)"},
		"a 200":      {answer{status: http.StatusOK}, false, "E_CLIENT_OPEN: auth-api serves /api/me to anyone (200)"},
		"a 204":      {answer{status: http.StatusNoContent}, false, "E_CLIENT_OPEN: auth-api serves /api/me to anyone (204)"},
		"no answer":  {answer{err: errUnanswered}, false, "auth-api did not answer /api/me, so it is not shown to refuse it"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &copyOf{answers: map[string]answer{"/api/me": tc.answer}}
			got, err := midRelease(t, signingIn(), gate.WithTraffic(fake)).MayPromote(t.Context(), asks())
			if err != nil || got.Yes != tc.yes || got.Why != tc.why {
				t.Fatalf("may-promote = %+v, %v", got, err)
			}
			if strings.Join(fake.asked, " ") != "http://auth-api-canary.auth-system.svc.cluster.local:8080/api/me" {
				t.Fatalf("asked %v", fake.asked)
			}
		})
	}
}

func TestTheProtectedPathIsAskedOfTheMemberThatServesIt(t *testing.T) {
	t.Run("another member that changed is asked at its own Canary service", func(t *testing.T) {
		c := signingIn()
		c.inputs = strings.Replace(nativeInputs, `"protectedBy":"auth-api"`, `"protectedBy":"auth-ui"`, 1)
		fake := &copyOf{answers: map[string]answer{"/api/me": {status: http.StatusUnauthorized}}}
		got, err := midRelease(t, c, gate.WithTraffic(fake)).MayPromote(t.Context(), asks())
		if err != nil || !got.Yes || strings.Join(fake.asked, " ") != "http://auth-ui-canary.auth-system.svc.cluster.local:8080/api/me" {
			t.Fatalf("may-promote = %+v, %v, asked %v", got, err, fake.asked)
		}
	})
	t.Run("a member that did not change is not asked", func(t *testing.T) {
		c := signingIn()
		c.inputs = strings.Replace(nativeInputs, `"protectedBy":"auth-api"`, `"protectedBy":"auth-ui"`, 1)
		c.canaries["auth-ui"] = of("Succeeded", "u1", "u1", 0)
		fake := &copyOf{}
		got, err := midRelease(t, c, gate.WithTraffic(fake)).MayPromote(t.Context(), asks())
		if err != nil || !got.Yes || got.Why != "every member passed its analysis or did not change, and auth-ui, which did not change, was asked when it last did" || len(fake.asked) != 0 {
			t.Fatalf("may-promote = %+v, %v, asked %v", got, err, fake.asked)
		}
	})
	t.Run("a member Flagger records as settled whose Deployment moved is asked", func(t *testing.T) {
		c := signingIn()
		c.inputs = strings.Replace(nativeInputs, `"protectedBy":"auth-api"`, `"protectedBy":"auth-ui"`, 1)
		settled := of("Succeeded", "u1", "u1", 0)
		settled.Port = 8080
		c.canaries["auth-ui"] = settled
		// A member whose Deployment changed a moment ago holds the barrier shut itself.
		c.changed["auth-ui"] = true
		fake := &copyOf{}
		if got, err := midRelease(t, c, gate.WithTraffic(fake)).MayPromote(t.Context(), asks()); err != nil || got.Yes || len(fake.asked) != 0 {
			t.Fatalf("may-promote = %+v, %v, asked %v", got, err, fake.asked)
		}
	})
	t.Run("nothing is asked while the barrier is shut", func(t *testing.T) {
		c := signingIn()
		c.canaries["auth-ui"] = of("Progressing", "u2", "u1", 1)
		fake := &copyOf{}
		if got, err := midRelease(t, c, gate.WithTraffic(fake)).MayPromote(t.Context(), asks()); err != nil || got.Yes || len(fake.asked) != 0 {
			t.Fatalf("may-promote = %+v, %v, asked %v", got, err, fake.asked)
		}
	})
}

func TestProtectedPathInputsTheGateCannotAnswerFrom(t *testing.T) {
	cases := map[string]struct {
		inputs string
		change func(*cluster)
	}{
		"no member named":     {strings.Replace(nativeInputs, `"protectedBy":"auth-api",`, ``, 1), nil},
		"a Process no member": {strings.Replace(nativeInputs, `"protectedBy":"auth-api"`, `"protectedBy":"auth-worker"`, 1), nil},
		"a path with a query": {strings.Replace(nativeInputs, `"/api/me"`, `"/api/me?x=1"`, 1), nil},
		"no service port":     {nativeInputs, func(c *cluster) { api := c.canaries["auth-api"]; api.Port = 0; c.canaries["auth-api"] = api }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := signingIn()
			c.inputs = tc.inputs
			if tc.change != nil {
				tc.change(c)
			}
			fake := &copyOf{}
			got, err := midRelease(t, c, gate.WithTraffic(fake)).MayPromote(t.Context(), asks())
			if err == nil || got.Yes || len(fake.asked) != 0 {
				t.Fatalf("may-promote = %+v, %v, asked %v", got, err, fake.asked)
			}
		})
	}
}
