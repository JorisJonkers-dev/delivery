package gate_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

const gateRBAC = "../../deploy/release-gate/rbac.yaml"

// canaries is as much of Flagger's Canary kind as the gate reads: any spec, any status.
func canaries() *apiextensionsv1.CustomResourceDefinition {
	open := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "canaries.flagger.app"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "flagger.app",
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "canaries", Singular: "canary", Kind: "Canary", ListKind: "CanaryList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1beta1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &open}},
			}},
		},
	}
}

// grant creates exactly what deploy/release-gate/rbac.yaml declares, and returns the
// ServiceAccount it declares as the user a pod running under it is.
func grant(t *testing.T, admin kubernetes.Interface) string {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile(gateRBAC)
	if err != nil {
		t.Fatal(err)
	}
	var user string
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var encoded runtime.RawExtension
		if err := decoder.Decode(&encoded); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		object, _, err := scheme.Codecs.UniversalDeserializer().Decode(encoded.Raw, nil, nil)
		if err != nil {
			t.Fatalf("%s holds something that is not a Kubernetes object: %v", gateRBAC, err)
		}
		switch o := object.(type) {
		case *corev1.ServiceAccount:
			user = "system:serviceaccount:" + o.Namespace + ":" + o.Name
		case *rbacv1.ClusterRole:
			_, err = admin.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.ClusterRoleBinding:
			_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
		default:
			t.Fatalf("%s grants through a %T, which this test does not apply", gateRBAC, object)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if user == "" {
		t.Fatalf("%s declares no ServiceAccount", gateRBAC)
	}
	return user
}

// scene is one Application in a namespace of its own, at some point of a release.
type scene struct {
	inputs string
	// record is what the gate recorded before; nil where it recorded nothing.
	record *gate.Record
	// apiPhase is Flagger's phase for the API, which changed in this release; uiPromoted its
	// digest of the UI's primary, which did not.
	apiPhase   string
	uiPromoted string
	// upDone is whether the migration has run; newCopy whether a pod of the API's new copy is left.
	upDone  bool
	newCopy bool
}

func workload(namespace, name, image string) *appsv1.Deployment {
	selector := map[string]string{"app.kubernetes.io/instance": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image}}},
			},
		},
	}
}

func releaseJob(namespace, name string) *batchv1.Job {
	suspended := true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels("auth-migration", "auth-migration")},
		Spec: batchv1.JobSpec{
			Suspend: &suspended,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "migration", Image: "ghcr.io/x/auth-migration@sha256:aa"}},
			}},
		},
	}
}

// stage puts the scene into the cluster as the render, Flagger and the Jobs' controller would.
func stage(t *testing.T, admin kubernetes.Interface, dyn dynamic.Interface, namespace string, s scene) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", namespace, err)
		}
	}
	_, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	must(err)
	// A record forged beside the Application, saying the proven revision serves, is not read.
	_, err = admin.CoreV1().ConfigMaps(namespace).Create(ctx, configMap(namespace, "auth-release-record", nil, map[string]string{"record.json": `{"namespace":"` + namespace + `","serving":"` + earlier + `"}`}), metav1.CreateOptions{})
	must(err)
	_, err = admin.CoreV1().ConfigMaps(namespace).Create(ctx, configMap(namespace, "auth-release-gate", renderedFor("auth"), map[string]string{"releaseGate.json": s.inputs}), metav1.CreateOptions{})
	must(err)
	if s.record != nil {
		must(gate.Kube{Client: admin}.SetRecord(ctx, namespace, "auth", *s.record))
	}
	for name, image := range map[string]string{"auth-api": "api:2", "auth-api-primary": "api:1", "auth-ui": "ui:1", "auth-ui-primary": "ui:1"} {
		_, err = admin.AppsV1().Deployments(namespace).Create(ctx, workload(namespace, name, image), metav1.CreateOptions{})
		must(err)
	}
	for name, status := range map[string]map[string]any{
		"auth-api": {"phase": s.apiPhase, "lastAppliedSpec": "a2", "lastPromotedSpec": "a1", "lastTransitionTime": first.Add(30 * time.Second).Format(time.RFC3339)},
		"auth-ui":  {"phase": "Succeeded", "lastAppliedSpec": "u1", "lastPromotedSpec": s.uiPromoted},
	} {
		object := canary(name, rendered(revision), status)
		object.SetNamespace(namespace)
		_, err = dyn.Resource(gate.Canaries).Namespace(namespace).Create(ctx, object, metav1.CreateOptions{})
		must(err)
	}
	for _, name := range []string{upJob, downJob} {
		_, err = admin.BatchV1().Jobs(namespace).Create(ctx, releaseJob(namespace, name), metav1.CreateOptions{})
		must(err)
	}
	if s.upDone {
		done := metav1.NewTime(first)
		up, err := admin.BatchV1().Jobs(namespace).Get(ctx, upJob, metav1.GetOptions{})
		must(err)
		started := false
		up.Spec.Suspend = &started
		up, err = admin.BatchV1().Jobs(namespace).Update(ctx, up, metav1.UpdateOptions{})
		must(err)
		up.Status = batchv1.JobStatus{
			StartTime: &done, CompletionTime: &done, Succeeded: 1,
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: done},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: done},
			},
		}
		_, err = admin.BatchV1().Jobs(namespace).UpdateStatus(ctx, up, metav1.UpdateOptions{})
		must(err)
	}
	if s.newCopy {
		_, err = admin.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "auth-api-7c9d-x", Namespace: namespace, Labels: labels("auth-api", "auth-api")},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "auth-api", Image: "api:2"}}},
		}, metav1.CreateOptions{})
		must(err)
	}
}

func TestTheGateInACluster(t *testing.T) {
	environment := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{canaries()}}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start a control plane (task test sets KUBEBUILDER_ASSETS): %v", err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	ctx := context.Background()
	admin, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	// The gate's own view of the cluster: the ServiceAccount of the manifest, and nothing more.
	asGate := rest.CopyConfig(config)
	asGate.Impersonate = rest.ImpersonationConfig{UserName: grant(t, admin)}
	restricted, err := kubernetes.NewForConfig(asGate)
	if err != nil {
		t.Fatal(err)
	}
	restrictedDynamic, err := dynamic.NewForConfig(asGate)
	if err != nil {
		t.Fatal(err)
	}

	held := func() *gate.Record { r := serving(); r.Pinned, r.Since = revision, first; return r }
	nonTransactional := strings.Replace(migratingInputs, `"nonTransactional":false`, `"nonTransactional":true`, 1)
	scenes := map[string]scene{
		"proof-holds":   {inputs: migratingInputs, record: serving(), apiPhase: "Progressing", uiPromoted: "u1"},
		"first-release": {inputs: firstInputs, apiPhase: "Progressing", uiPromoted: "u1"},
		// The record beside it says the proven revision serves; the gate's own says another does.
		"stale-proof":   {inputs: migratingInputs, record: &gate.Record{Serving: "sha256:ffff", Promoted: serving().Promoted}, apiPhase: "Progressing", uiPromoted: "u1"},
		"down":          {inputs: migratingInputs, record: held(), apiPhase: "Failed", uiPromoted: "u1", upDone: true},
		"down-running":  {inputs: migratingInputs, record: held(), apiPhase: "Failed", uiPromoted: "u1", upDone: true, newCopy: true},
		"down-promoted": {inputs: migratingInputs, record: held(), apiPhase: "Failed", uiPromoted: "u2", upDone: true},
		"down-ddl":      {inputs: nonTransactional, record: held(), apiPhase: "Failed", uiPromoted: "u1", upDone: true},
		"down-not-held": {inputs: migratingInputs, record: held(), apiPhase: "Progressing", uiPromoted: "u1", upDone: true},
	}
	// Every scene is an Application named auth, and inputs under one id in two namespaces are
	// tended in neither. So the scenes are tended one at a time, each cleared away after.
	_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: gate.RecordNamespace}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g := gate.New(gate.Kube{Client: restricted, Dynamic: restrictedDynamic})
	var found gate.Standing
	records := map[string]gate.Record{}
	for _, namespace := range slices.Sorted(maps.Keys(scenes)) {
		stage(t, admin, dyn, namespace, scenes[namespace])
		if err := g.Tend(ctx, now, quiet); err != nil {
			t.Fatal(err)
		}
		round := g.Standing()
		if round.Unanswerable != nil {
			t.Fatalf("the gate could not read %+v with what its manifest grants", round.Unanswerable)
		}
		found.Held = append(found.Held, round.Held...)
		found.Refused = append(found.Refused, round.Refused...)
		if record, err := (gate.Kube{Client: admin}).Record(ctx, namespace, "auth"); err == nil {
			records[namespace] = record
		}
		for _, cleared := range []error{
			admin.CoreV1().ConfigMaps(namespace).Delete(ctx, "auth-release-gate", metav1.DeleteOptions{}),
			admin.CoreV1().ConfigMaps(gate.RecordNamespace).Delete(ctx, gate.RecordName(namespace, "auth"), metav1.DeleteOptions{}),
		} {
			if cleared != nil && !apierrors.IsNotFound(cleared) {
				t.Fatal(cleared)
			}
		}
	}

	suspended := func(t *testing.T, namespace, name string) bool {
		t.Helper()
		job, err := admin.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return *job.Spec.Suspend
	}
	heldFor := func(namespace string) string {
		for _, h := range found.Held {
			if h.Namespace == namespace {
				return h.Reason
			}
		}
		return ""
	}
	refusedFor := func(namespace string) string {
		for _, r := range found.Refused {
			if r.Namespace == namespace {
				return r.Condition
			}
		}
		return ""
	}

	t.Run("a migration whose proof holds is started, and one proven against nothing where nothing serves", func(t *testing.T) {
		for _, namespace := range []string{"proof-holds", "first-release"} {
			if suspended(t, namespace, upJob) || !suspended(t, namespace, downJob) || heldFor(namespace) != "" {
				t.Fatalf("%s: up suspended %v, down suspended %v, held %q", namespace, suspended(t, namespace, upJob), suspended(t, namespace, downJob), heldFor(namespace))
			}
		}
		// The gate noted the release where it had recorded nothing, in its own namespace.
		if record := records["first-release"]; record.Pinned != revision || !record.Since.Equal(now) || record.Serving != "" {
			t.Fatalf("record = %+v", record)
		}
	})

	t.Run("a stale proof holds the release with the schema untouched", func(t *testing.T) {
		if !suspended(t, "stale-proof", upJob) || !suspended(t, "stale-proof", downJob) {
			t.Fatal("a Job of a release whose proof went stale was started")
		}
		if heldFor("stale-proof") != gate.HeldStaleProof || refusedFor("stale-proof") != "" {
			t.Fatalf("held %q, refused %q", heldFor("stale-proof"), refusedFor("stale-proof"))
		}
	})

	t.Run("the Down starts under every one of its conditions, and under no fewer", func(t *testing.T) {
		cases := map[string]struct {
			down    bool
			held    string
			refused string
		}{
			"down":          {true, gate.HeldAnalysis, ""},
			"down-running":  {false, gate.HeldAnalysis, gate.UndoNewCopyRunning},
			"down-promoted": {false, gate.HeldAnalysis, gate.UndoPrimariesMoved},
			"down-ddl":      {false, gate.HeldAnalysis, gate.UndoNonTransactional},
			"down-not-held": {false, "", ""},
		}
		for namespace, want := range cases {
			if started := !suspended(t, namespace, downJob); started != want.down || heldFor(namespace) != want.held || refusedFor(namespace) != want.refused {
				t.Errorf("%s: down started %v, held %q, refused %q", namespace, started, heldFor(namespace), refusedFor(namespace))
			}
		}
	})

	t.Run("a Job changed since the gate read it is not the one started", func(t *testing.T) {
		k := gate.Kube{Client: restricted, Dynamic: restrictedDynamic}
		jobs, err := k.Jobs(ctx, "down-not-held", "auth")
		if err != nil {
			t.Fatal(err)
		}
		var read gate.Job
		for _, job := range jobs {
			if job.Name == downJob {
				read = job
			}
		}
		changed, err := admin.BatchV1().Jobs("down-not-held").Get(ctx, downJob, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		changed.Labels["changed"] = "since"
		if _, err := admin.BatchV1().Jobs("down-not-held").Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := k.Start(ctx, "down-not-held", read); !apierrors.IsConflict(err) {
			t.Fatalf("start of a Job changed since it was read = %v", err)
		}
		if !suspended(t, "down-not-held", downJob) {
			t.Fatal("a Job changed since it was read was started")
		}
	})

	t.Run("its manifest lets it write its record and a Job's suspend, and nothing it only reads", func(t *testing.T) {
		forbidden := map[string]error{}
		forbidden["delete a ConfigMap"] = restricted.CoreV1().ConfigMaps("down").Delete(ctx, "auth-release-gate", metav1.DeleteOptions{})
		forbidden["delete a Job"] = restricted.BatchV1().Jobs("down").Delete(ctx, upJob, metav1.DeleteOptions{})
		_, forbidden["read a Secret"] = restricted.CoreV1().Secrets("down").Get(ctx, "any", metav1.GetOptions{})
		_, forbidden["create a Job"] = restricted.BatchV1().Jobs("down").Create(ctx, releaseJob("down", "another"), metav1.CreateOptions{})
		_, forbidden["change a Deployment"] = restricted.AppsV1().Deployments("down").Update(ctx, workload("down", "auth-api", "api:3"), metav1.UpdateOptions{})
		_, forbidden["change a Canary"] = restrictedDynamic.Resource(gate.Canaries).Namespace("down").Update(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "flagger.app/v1beta1", "kind": "Canary", "metadata": map[string]any{"name": "auth-api", "namespace": "down"},
		}}, metav1.UpdateOptions{})
		for _, what := range slices.Sorted(maps.Keys(forbidden)) {
			if !apierrors.IsForbidden(forbidden[what]) {
				t.Errorf("the gate may %s: %v", what, forbidden[what])
			}
		}
	})
}
