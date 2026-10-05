package gate_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

func configMap(namespace, name string, labels, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}, Data: data}
}

func renderedFor(application string) map[string]string {
	return map[string]string{"app.kubernetes.io/part-of": application, "app.kubernetes.io/managed-by": "deploy-kit"}
}

func TestTheGatedApplicationsAreTheOnesWhoseInputsTheRenderWrote(t *testing.T) {
	k := kube([]runtime.Object{
		configMap(ns, "auth-release-gate", renderedFor("auth"), nil),
		configMap("mail-system", "mail-release-gate", renderedFor("mail"), nil),
		// Another ConfigMap of a gated Application is not its inputs.
		configMap(ns, "auth-api-config", renderedFor("auth"), nil),
		// A ConfigMap that only carries the name, written by someone else, is not the render's.
		configMap("stray", "auth-release-gate", map[string]string{"app.kubernetes.io/part-of": "auth"}, nil),
		// Nor is one the render wrote for no Application, or under another Application's name.
		configMap("odd", "-release-gate", map[string]string{"app.kubernetes.io/managed-by": "deploy-kit"}, nil),
		configMap("odd", "auth-release-gate", renderedFor("mail"), nil),
		// The gate's own record is not something to tend.
		configMap(ns, "auth-system.auth-release-record", map[string]string{"app.kubernetes.io/part-of": "auth", "app.kubernetes.io/managed-by": "release-gate"}, nil),
	})

	got, err := k.Applications(t.Context())

	want := []gate.Gated{{Namespace: ns, Application: "auth"}, {Namespace: "mail-system", Application: "mail"}}
	slices.SortFunc(got, func(a, b gate.Gated) int { return strings.Compare(a.Namespace, b.Namespace) })
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("applications = %+v, %v", got, err)
	}
}

func TestTheRecordIsAConfigMapTheGateCreatesAndThenReplacesInItsOwnNamespace(t *testing.T) {
	k := kube(nil)
	if _, err := k.Record(t.Context(), ns, "auth"); !errors.Is(err, gate.ErrNoRecord) {
		t.Fatalf("an Application with no record = %v", err)
	}
	since := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := k.SetRecord(t.Context(), ns, "auth", gate.Record{Pinned: revision, Since: since}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Client.CoreV1().ConfigMaps(ns).Get(t.Context(), "auth-system.auth-release-record", metav1.GetOptions{}); err == nil {
		t.Fatal("the record was written beside the Application, where it could write it too")
	}
	stored, err := k.Client.CoreV1().ConfigMaps(gate.RecordNamespace).Get(t.Context(), "auth-system.auth-release-record", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantLabels := map[string]string{"app.kubernetes.io/part-of": "auth", "app.kubernetes.io/managed-by": "release-gate"}
	wantData := map[string]string{"record.json": `{"namespace":"` + ns + `","pinned":"` + revision + `","since":"2026-10-05T12:00:00Z"}`}
	if gate.RecordNamespace != "delivery-system" || !maps.Equal(stored.Labels, wantLabels) || !maps.Equal(stored.Data, wantData) {
		t.Fatalf("stored = %v %v", stored.Labels, stored.Data)
	}

	second := gate.Record{Serving: revision, Promoted: map[string]string{"auth-api": "a2"}}
	if err := k.SetRecord(t.Context(), ns, "auth", second); err != nil {
		t.Fatal(err)
	}
	got, err := k.Record(t.Context(), ns, "auth")
	if err != nil || got.Serving != revision || got.Namespace != ns || !maps.Equal(got.Promoted, second.Promoted) || got.Pinned != "" || !got.Since.IsZero() {
		t.Fatalf("record = %+v, %v", got, err)
	}
	// An Application of the same id in another namespace has a record of its own, or none.
	if got, err := k.Record(t.Context(), "elsewhere", "auth"); !errors.Is(err, gate.ErrNoRecord) {
		t.Fatalf("another namespace read %+v, %v", got, err)
	}
}

func TestARecordIsNeverClaimedOrWrittenOverFromAnotherNamespace(t *testing.T) {
	k := kube(nil)
	// Inputs under auth's id elsewhere, tended first, write a record of their own namespace.
	if err := k.SetRecord(t.Context(), "elsewhere", "auth", gate.Record{Serving: earlier}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Record(t.Context(), ns, "auth"); !errors.Is(err, gate.ErrNoRecord) {
		t.Fatalf("auth's record was claimed from another namespace: %v", err)
	}
	if err := k.SetRecord(t.Context(), ns, "auth", gate.Record{Serving: revision}); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Record(t.Context(), ns, "auth"); err != nil || got.Serving != revision {
		t.Fatalf("record = %+v, %v", got, err)
	}
	// A ConfigMap planted under auth's record's name, naming another namespace, is neither read
	// nor written over; nor is one that does not parse.
	for name, planted := range map[string]string{
		"another namespace": `{"namespace":"elsewhere","serving":"` + earlier + `"}`,
		"not JSON":          "{",
	} {
		k := kube([]runtime.Object{configMap(gate.RecordNamespace, "auth-system.auth-release-record", nil, map[string]string{"record.json": planted})})
		if _, err := k.Record(t.Context(), ns, "auth"); err == nil || errors.Is(err, gate.ErrNoRecord) {
			t.Fatalf("%s: read as auth's", name)
		}
		if err := k.SetRecord(t.Context(), ns, "auth", gate.Record{Serving: revision}); err == nil {
			t.Fatalf("%s: written over", name)
		}
	}
}

func TestARecordThatDoesNotParseIsNoRecordTheGateCanRead(t *testing.T) {
	for name, data := range map[string]map[string]string{
		"not JSON":          {"record.json": "{"},
		"without a key":     {"other": "{}"},
		"with no data":      nil,
		"not an object":     {"record.json": `"sha256:1"`},
		"a wrong member":    {"record.json": `{"namespace":"` + ns + `","serving":7}`},
		"with no namespace": {"record.json": `{"serving":"` + revision + `"}`},
	} {
		k := kube([]runtime.Object{configMap(gate.RecordNamespace, "auth-system.auth-release-record", nil, data)})
		if got, err := k.Record(t.Context(), ns, "auth"); err == nil || errors.Is(err, gate.ErrNoRecord) {
			t.Fatalf("%s: read as %+v, %v", name, got, err)
		}
	}
}

func TestStartingAJobUnsuspendsItAndNothingElse(t *testing.T) {
	suspended := true
	limit := int32(0)
	k := kube([]runtime.Object{&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-migration-9d2c4e6a8b0d", Namespace: ns, Labels: labels("auth-migration", "auth-migration")},
		Spec:       batchv1.JobSpec{Suspend: &suspended, BackoffLimit: &limit},
	}})
	read := func() gate.Job {
		jobs, err := k.Jobs(t.Context(), ns, "auth")
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %+v, %v", jobs, err)
		}
		return jobs[0]
	}
	if !read().Suspended {
		t.Fatal("a Job the render applied suspended is read as started")
	}

	if err := k.Start(t.Context(), ns, read()); err != nil {
		t.Fatal(err)
	}

	stored, err := k.Client.BatchV1().Jobs(ns).Get(t.Context(), "auth-migration-9d2c4e6a8b0d", metav1.GetOptions{})
	if err != nil || read().Suspended || *stored.Spec.Suspend || *stored.Spec.BackoffLimit != 0 {
		t.Fatalf("after start: %+v, %v", stored.Spec, err)
	}
	if err := k.Start(t.Context(), ns, gate.Job{Name: "absent"}); err == nil {
		t.Fatal("a Job that is not there was started")
	}
}

func TestACanarysLastTransitionIsReadWhereFlaggerWroteOne(t *testing.T) {
	k := kube(nil,
		canary("auth-api", rendered(revision), map[string]any{"phase": "Failed", "lastTransitionTime": "2026-10-05T12:00:30Z"}),
		canary("auth-ui", rendered(earlier), map[string]any{"phase": "Failed", "lastTransitionTime": "yesterday"}),
	)

	api, err := k.Canary(t.Context(), ns, "auth-api")
	if err != nil || !api.Transitioned.Equal(time.Date(2026, 10, 5, 12, 0, 30, 0, time.UTC)) {
		t.Fatalf("canary = %+v, %v", api, err)
	}
	// A time the gate cannot read is no time: it places nothing after anything.
	if ui, err := k.Canary(t.Context(), ns, "auth-ui"); err != nil || !ui.Transitioned.IsZero() {
		t.Fatalf("canary = %+v, %v", ui, err)
	}
}
