package gate_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// flagger is the body Flagger posts to a webhook (flagger.app/v1beta1): the Canary's name and
// namespace, its phase, its checksum and build id, and the `metadata` the Canary's webhook
// declares, which is where the render puts the Application, the Process and the revision.
func flagger(process, phase string) string {
	return fmt.Sprintf(`{"name":%q,"namespace":"auth-system","phase":%q,"checksum":"85d557f47b","buildId":"","metadata":{"application":"auth","process":%q,"revision":%q}}`,
		process, phase, process, revision)
}

func serve(t *testing.T, c gate.Cluster) (*httptest.Server, *strings.Builder) {
	t.Helper()
	logged := &strings.Builder{}
	mux := http.NewServeMux()
	gate.New(c).Routes(mux, slog.New(slog.NewTextHandler(logged, nil)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, logged
}

func post(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	said, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, strings.TrimSpace(string(said))
}

func TestFlaggersThreeWebhooksAreAnsweredByStatus(t *testing.T) {
	through := auth()
	through.canaries["auth-ui"] = of("WaitingPromotion", "u2", "u1", 4)
	waiting := auth()
	waiting.jobs = []gate.Job{{Name: "auth-migration-9d2c4e6a8b0d", Component: "auth-migration"}}
	waiting.pods["auth-api"] = []gate.Pod{{Name: "auth-api-7c9d-x"}}

	cases := []struct {
		name    string
		cluster *cluster
		path    string
		phase   string
		status  int
		said    string
	}{
		{"confirm-rollout, nothing to wait for", through, gate.PathMayStart, "Progressing", http.StatusOK, "the release has no migration and no prepare Process"},
		{"rollout, the new copy healthy", through, gate.PathChecks, "Progressing", http.StatusOK, "every pod of the new copy is ready and none restarted"},
		{"confirm-promotion, every member through", through, gate.PathMayPromote, "Progressing", http.StatusOK, "every member passed its analysis or did not change"},
		{"confirm-rollout, the migration not run", waiting, gate.PathMayStart, "Progressing", http.StatusConflict, "auth-migration-9d2c4e6a8b0d has not completed"},
		{"rollout, the new copy not ready", waiting, gate.PathChecks, "Progressing", http.StatusConflict, "auth-api-7c9d-x is not ready"},
		{"confirm-promotion, at the barrier", waiting, gate.PathMayPromote, "WaitingPromotion", http.StatusConflict, "auth-ui has not passed its analysis (Progressing)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, logged := serve(t, tc.cluster)
			status, said := post(t, srv, tc.path, flagger("auth-api", tc.phase))
			if status != tc.status || said != tc.said {
				t.Fatalf("%s = %d %q", tc.path, status, said)
			}
			if !strings.Contains(logged.String(), "release gate answered") || !strings.Contains(logged.String(), tc.path) {
				t.Fatalf("the answer is not logged: %s", logged)
			}
		})
	}
}

func TestTheBarrierOpensForEveryMemberOnceTheLastOneArrives(t *testing.T) {
	// Flagger's own order: the API finishes its analysis first and is refused, which is what
	// makes Flagger mark it as waiting; then the UI finishes, and both are let through.
	c := auth()
	srv, _ := serve(t, c)

	if status, _ := post(t, srv, gate.PathMayPromote, flagger("auth-api", "Progressing")); status != http.StatusConflict {
		t.Fatalf("the API, with the UI still analysed = %d", status)
	}
	c.canaries["auth-api"] = of("WaitingPromotion", "a2", "a1", 4)
	c.canaries["auth-ui"] = of("Progressing", "u2", "u1", 4)
	if status, _ := post(t, srv, gate.PathMayPromote, flagger("auth-ui", "Progressing")); status != http.StatusOK {
		t.Fatalf("the UI, last to arrive = %d", status)
	}
	c.canaries["auth-ui"] = of("Promoting", "u2", "u1", 4)
	if status, _ := post(t, srv, gate.PathMayPromote, flagger("auth-api", "WaitingPromotion")); status != http.StatusOK {
		t.Fatalf("the API, once the UI is through = %d", status)
	}
}

func TestAQuestionTheGateCannotAnswerIsNoAndSaysNothingOfWhy(t *testing.T) {
	for _, path := range []string{gate.PathMayStart, gate.PathChecks, gate.PathMayPromote} {
		c := auth()
		c.away = "inputs"
		srv, logged := serve(t, c)
		status, said := post(t, srv, path, flagger("auth-api", "Progressing"))
		if status != http.StatusServiceUnavailable || said != "the gate could not answer, so the answer is no" {
			t.Fatalf("%s = %d %q", path, status, said)
		}
		if !strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), "the API server is away") || strings.Contains(logged.String(), "release gate answered") {
			t.Fatalf("the cause belongs in the log: %s", logged)
		}
	}
}

func TestAPayloadThatAsksNothingIsRefused(t *testing.T) {
	srv, _ := serve(t, auth())
	cases := map[string]string{
		"not JSON":             "name=auth-api",
		"no metadata":          `{"name":"auth-api","namespace":"auth-system","phase":"Progressing"}`,
		"no namespace":         `{"name":"auth-api","metadata":{"application":"auth","process":"auth-api","revision":"` + revision + `"}}`,
		"no Application":       `{"name":"auth-api","namespace":"auth-system","metadata":{"process":"auth-api","revision":"` + revision + `"}}`,
		"no Process":           `{"name":"auth-api","namespace":"auth-system","metadata":{"application":"auth","revision":"` + revision + `"}}`,
		"no revision":          `{"name":"auth-api","namespace":"auth-system","metadata":{"application":"auth","process":"auth-api"}}`,
		"another Canary's":     `{"name":"auth-ui","namespace":"auth-system","metadata":{"application":"auth","process":"auth-api","revision":"` + revision + `"}}`,
		"a body past the size": `{"name":"` + strings.Repeat("a", 70<<10) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{gate.PathMayStart, gate.PathChecks, gate.PathMayPromote} {
				if status, _ := post(t, srv, path, body); status != http.StatusBadRequest {
					t.Fatalf("%s = %d, want 400", path, status)
				}
			}
		})
	}
}

func TestOnlyAPostAsks(t *testing.T) {
	srv, _ := serve(t, auth())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+gate.PathMayPromote, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
}
