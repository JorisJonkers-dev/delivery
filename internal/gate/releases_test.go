package gate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// estate is a cluster of several gated Applications, each with inputs and a record of its own.
type estate struct {
	*cluster
	gated   []gate.Gated
	inputs  map[string]string
	records map[string]gate.Record
	// broken names the Applications whose record is not one the gate wrote for them.
	broken map[string]error
}

func (e *estate) Applications(context.Context) ([]gate.Gated, error) {
	if e.away == "applications" {
		return nil, errAway
	}
	return e.gated, nil
}

func (e *estate) Inputs(_ context.Context, namespace, application string) (string, error) {
	if e.away == "inputs" {
		return "", errAway
	}
	raw, held := e.inputs[namespace+"/"+application]
	if !held {
		return "", fmt.Errorf("%w: configmaps %q not found", gate.ErrNoInputs, application+"-release-gate")
	}
	return raw, nil
}

func (e *estate) Record(_ context.Context, namespace, application string) (gate.Record, error) {
	key := namespace + "/" + application
	if err := e.broken[key]; err != nil {
		return gate.Record{}, err
	}
	if e.away == "record" {
		return gate.Record{}, errAway
	}
	record, held := e.records[key]
	if !held {
		return gate.Record{}, gate.ErrNoRecord
	}
	return record, nil
}

const notesInputs = `{"analysis":{"interval":"30s","iterations":4,"threshold":3,"errorRate":"1%","latency":"500ms"},"deadline":"1800s","endpoint":"http://release-gate.delivery-system.svc.cluster.local:8080","members":[{"process":"notes-api","readiness":{"path":"/readyz","port":8080}}],"migration":{"identity":"notes-migration","testedAgainst":"sha256:0a1b","nonTransactional":true}}`

func anEstate() *estate {
	since := time.Date(2026, 10, 14, 17, 36, 0, 0, time.FixedZone("CEST", 2*60*60))
	return &estate{
		cluster: auth(),
		gated: []gate.Gated{
			{Namespace: "notes-system", Application: "notes"},
			{Namespace: "auth-system", Application: "auth"},
			{Namespace: "wiki-system", Application: "wiki"},
			{Namespace: "blog-system", Application: "blog"},
			{Namespace: "shop-system", Application: "shop"},
		},
		inputs: map[string]string{
			"auth-system/auth":   authInputs,
			"notes-system/notes": notesInputs,
			"blog-system/blog":   "{not json",
			"shop-system/shop":   authInputs,
		},
		records: map[string]gate.Record{
			"auth-system/auth": {Namespace: "auth-system", Serving: earlier, Pinned: revision, Since: since},
		},
		broken: map[string]error{},
	}
}

func getReleases(t *testing.T, c gate.Cluster) (int, string) {
	t.Helper()
	srv, _ := serve(t, c)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+gate.PathReleases, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	said, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK && resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, strings.TrimSpace(string(said))
}

func TestTheGateServesWhatItHoldsOfEveryGatedApplication(t *testing.T) {
	e := anEstate()
	// Shop's record is planted: it names another namespace than shop's.
	e.broken["shop-system/shop"] = fmt.Errorf("%w: it names another namespace", gate.ErrForeignRecord)
	status, body := getReleases(t, e)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	want := `{"releases":[` +
		`{"namespace":"auth-system","application":"auth","members":["auth-api","auth-ui"],` +
		`"serving":"` + earlier + `","pinned":"` + revision + `","since":"2026-10-14T15:36:00Z"},` +
		`{"namespace":"blog-system","application":"blog","unreadable":"release-gate inputs that do not parse"},` +
		`{"namespace":"notes-system","application":"notes","members":["notes-api"],` +
		`"migration":{"identity":"notes-migration","testedAgainst":"sha256:0a1b","nonTransactional":true}},` +
		`{"namespace":"shop-system","application":"shop","members":["auth-api","auth-ui"],"unreadable":"a release record the gate did not write for it"},` +
		`{"namespace":"wiki-system","application":"wiki","unreadable":"no release-gate inputs"}` +
		`],"truncated":false}`
	if body != want {
		t.Fatalf("body\n%s\nwant\n%s", body, want)
	}
}

func TestNoGatedApplicationIsAnEmptyListNotNull(t *testing.T) {
	e := anEstate()
	e.gated = nil
	if status, body := getReleases(t, e); status != http.StatusOK || body != `{"releases":[],"truncated":false}` {
		t.Fatalf("%d %s", status, body)
	}
}

func TestOneAnswerCarriesAThousandApplicationsAtMost(t *testing.T) {
	e := anEstate()
	e.gated = nil
	for i := range 1001 {
		e.gated = append(e.gated, gate.Gated{Namespace: fmt.Sprintf("p%04d-system", i), Application: "app"})
	}
	_, body := getReleases(t, e)
	var got gate.Releases
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Releases) != 1000 || !got.Truncated || got.Releases[999].Namespace != "p0999-system" {
		t.Fatalf("%d releases, truncated %v", len(got.Releases), got.Truncated)
	}
}

func TestAClusterThatDoesNotAnswerIsA503ThatSaysNothingOfWhy(t *testing.T) {
	for _, away := range []string{"applications", "inputs", "record"} {
		t.Run(away, func(t *testing.T) {
			e := anEstate()
			e.away = away
			srv, logged := serve(t, e)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+gate.PathReleases, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			said, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(said), "away") {
				t.Fatalf("%d %s", resp.StatusCode, said)
			}
			if !strings.Contains(logged.String(), "the API server is away") {
				t.Fatalf("the cause was not logged: %s", logged.String())
			}
		})
	}
}

func TestReadingTheReleasesStartsNothingAndWritesNothing(t *testing.T) {
	e := anEstate()
	if status, _ := getReleases(t, e); status != http.StatusOK {
		t.Fatal(status)
	}
	if len(e.written) != 0 || len(e.started) != 0 {
		t.Fatalf("wrote %v, started %v", e.written, e.started)
	}
}

func TestOnlyAGetReadsTheReleases(t *testing.T) {
	srv, _ := serve(t, anEstate())
	if status, _ := post(t, srv, gate.PathReleases, "{}"); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", status)
	}
}
