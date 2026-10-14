package reporter_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
	"github.com/JorisJonkers-dev/delivery/internal/reporter"
)

const rbacFile = "../../deploy/reporter/rbac.yaml"

// openKind is as much of a custom kind as the Reporter reads: any spec, any status.
func openKind(gvr schema.GroupVersionResource, kind string) *apiextensionsv1.CustomResourceDefinition {
	open := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: gvr.Resource, Kind: kind, ListKind: kind + "List"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &open}},
			}},
		},
	}
}

// applyRBAC creates exactly what deploy/reporter/rbac.yaml declares, and returns the
// ServiceAccount it declares as the user a pod running under it is.
func applyRBAC(t *testing.T, admin kubernetes.Interface) string {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile(rbacFile)
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
			t.Fatalf("%s holds something that is not a Kubernetes object: %v", rbacFile, err)
		}
		switch o := object.(type) {
		case *corev1.ServiceAccount:
			user = "system:serviceaccount:" + o.Namespace + ":" + o.Name
		case *rbacv1.ClusterRole:
			_, err = admin.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
		case *rbacv1.ClusterRoleBinding:
			_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
		default:
			t.Fatalf("%s grants through a %T, which this test does not apply", rbacFile, object)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if user == "" {
		t.Fatalf("%s declares no ServiceAccount", rbacFile)
	}
	return user
}

func create(t *testing.T, objects dynamic.Interface, gvr schema.GroupVersionResource, object map[string]any) {
	t.Helper()
	u := &unstructured.Unstructured{Object: object}
	if _, err := objects.Resource(gvr).Namespace(u.GetNamespace()).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func workload(namespace, name, image string) *appsv1.Deployment {
	selector := map[string]string{"app.kubernetes.io/instance": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
			},
		},
	}
}

func TestTheReporterInACluster(t *testing.T) {
	kinds := map[string]schema.GroupVersionResource{"Kustomization": reporter.Kustomizations, "OCIRepository": reporter.OCIRepositories, "Canary": gate.Canaries}
	environment := &envtest.Environment{}
	for kind, gvr := range kinds {
		environment.CRDs = append(environment.CRDs, openKind(gvr, kind))
	}
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
	adminObjects, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	user := applyRBAC(t, admin)
	asReporter := rest.CopyConfig(config)
	asReporter.Impersonate = rest.ImpersonationConfig{UserName: user}
	restricted, err := kubernetes.NewForConfig(asReporter)
	if err != nil {
		t.Fatal(err)
	}
	restrictedObjects, err := dynamic.NewForConfig(asReporter)
	if err != nil {
		t.Fatal(err)
	}

	for _, namespace := range []string{"flux-system", "notes-system"} {
		if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	managed := map[string]any{"app.kubernetes.io/managed-by": "deploy-kit"}
	create(t, adminObjects, reporter.Kustomizations, map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{
			"name": "apps-notes", "namespace": "flux-system", "labels": managed,
			"annotations": map[string]any{
				"event.toolkit.fluxcd.io/repository":  "JorisJonkers-dev/notes",
				"event.toolkit.fluxcd.io/sha":         "33a64302df71e989c018f6aa4602a8cb070dc739",
				"event.toolkit.fluxcd.io/environment": "production",
				"event.toolkit.fluxcd.io/version":     "1.4.2",
				"event.toolkit.fluxcd.io/url":         "https://notes.jorisjonkers.dev",
			},
		},
		"spec": map[string]any{"sourceRef": map[string]any{"kind": "OCIRepository", "name": "project-notes"}, "path": "./apps/notes"},
		"status": map[string]any{
			"lastAppliedRevision": pinned, "lastAttemptedRevision": pinned,
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "message": "Applied revision: " + pinned}},
			"inventory": map[string]any{"entries": []any{
				map[string]any{"id": "_notes-system__Namespace", "v": "v1"},
				map[string]any{"id": "notes-system_notes-api_apps_Deployment", "v": "v1"},
				map[string]any{"id": "notes-system_notes-api_flagger.app_Canary", "v": "v1beta1"},
				map[string]any{"id": "notes-system_notes-web_flagger.app_Canary", "v": "v1beta1"},
			}},
		},
	})
	// The estate's own unit: no Rollout, nothing more is read for it.
	create(t, adminObjects, reporter.Kustomizations, map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "estate-vso-secrets", "namespace": "flux-system", "labels": managed},
		"spec":     map[string]any{"sourceRef": map[string]any{"kind": "OCIRepository", "name": "estate"}},
	})
	// Flux's own unit, which deploy-kit did not render: not listed at all.
	create(t, adminObjects, reporter.Kustomizations, map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "flux-system", "namespace": "flux-system"},
		"spec":     map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}},
	})
	create(t, adminObjects, reporter.OCIRepositories, map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
		"metadata": map[string]any{"name": "project-notes", "namespace": "flux-system"},
		"spec":     map[string]any{"url": "oci://ghcr.io/jorisjonkers-dev/render/notes", "ref": map[string]any{"digest": pinned}},
	})
	create(t, adminObjects, gate.Canaries, map[string]any{
		"apiVersion": "flagger.app/v1beta1", "kind": "Canary",
		"metadata": map[string]any{"name": "notes-api", "namespace": "notes-system"},
		"spec":     map[string]any{"analysis": map[string]any{"iterations": int64(5)}},
		"status": map[string]any{
			"phase": "Progressing", "iterations": int64(2), "lastAppliedSpec": "h2", "lastPromotedSpec": "h1",
			"lastTransitionTime": "2026-10-14T17:40:00Z",
			"conditions":         []any{map[string]any{"type": "Promoted", "status": "Unknown", "message": "New revision detected, progressing canary analysis."}},
		},
	})
	create(t, adminObjects, gate.Canaries, map[string]any{
		"apiVersion": "flagger.app/v1beta1", "kind": "Canary",
		"metadata": map[string]any{"name": "notes-web", "namespace": "notes-system"},
		"spec":     map[string]any{"analysis": map[string]any{"iterations": int64(5)}},
		"status":   map[string]any{"phase": "Succeeded", "lastAppliedSpec": "h1", "lastPromotedSpec": "h1", "lastTransitionTime": "2026-10-01T09:00:00Z"},
	})
	for _, d := range []*appsv1.Deployment{workload("notes-system", "notes-web", "web:1"), workload("notes-system", "notes-web-primary", "web:1")} {
		if _, err := admin.AppsV1().Deployments(d.Namespace).Create(ctx, d, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	k := reporter.Kube{Client: restricted, Dynamic: restrictedObjects, Namespace: "flux-system"}

	t.Run("it reads each unit, its pin and its Canaries under its own grant", func(t *testing.T) {
		units, err := k.Units(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(units) != 2 {
			t.Fatalf("units = %+v", units)
		}
		var notes reporter.Unit
		for _, u := range units {
			if u.Name == "apps-notes" {
				notes = u
			} else if u.Rollout != (reporter.Rollout{}) || u.Members != nil {
				t.Fatalf("the estate's unit was read as a Rollout: %+v", u)
			}
		}
		want := reporter.Rollout{
			Repository: "JorisJonkers-dev/notes", SHA: "33a64302df71e989c018f6aa4602a8cb070dc739",
			Environment: "production", Version: "1.4.2", URL: "https://notes.jorisjonkers.dev",
		}
		if notes.Project != "notes" || notes.Rollout != want || notes.Pinned != pinned || notes.Applied != pinned || notes.Ready.Status != "True" {
			t.Fatalf("notes = %+v", notes)
		}
		if len(notes.Members) != 2 {
			t.Fatalf("members = %+v", notes.Members)
		}
		api, web := notes.Members[0], notes.Members[1]
		if api.Process != "notes-api" || api.Phase != "Progressing" || api.Iteration != 2 || api.Iterations != 5 ||
			api.Message != "New revision detected, progressing canary analysis." || api.Serves ||
			!api.Transitioned.Equal(time.Date(2026, 10, 14, 17, 40, 0, 0, time.UTC)) {
			t.Fatalf("notes-api = %+v", api)
		}
		if web.Process != "notes-web" || !reporter.Settled(web) || !web.Serves {
			t.Fatalf("notes-web = %+v", web)
		}
		stage, _ := reporter.StageOf(notes, began)
		if stage.Description != "applied by Flux; notes-api checks 2/5" {
			t.Fatalf("stage = %+v", stage)
		}
	})

	t.Run("a unit whose source or Canary is gone cannot be read", func(t *testing.T) {
		if err := adminObjects.Resource(gate.Canaries).Namespace("notes-system").Delete(ctx, "notes-api", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Units(ctx); !apierrors.IsNotFound(errors.Unwrap(errors.Unwrap(err))) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("the ServiceAccount can do nothing beyond its four reads", func(t *testing.T) {
		kustomizations := restrictedObjects.Resource(reporter.Kustomizations).Namespace("flux-system")
		canaries := restrictedObjects.Resource(gate.Canaries).Namespace("notes-system")
		repositories := restrictedObjects.Resource(reporter.OCIRepositories).Namespace("flux-system")
		deployments := restricted.AppsV1().Deployments("notes-system")
		object := func(kind string) *unstructured.Unstructured {
			return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "x/v1", "kind": kind, "metadata": map[string]any{"name": "intruder"}}}
		}
		refused := map[string]func() error{
			"get a Kustomization": func() error { _, err := kustomizations.Get(ctx, "apps-notes", metav1.GetOptions{}); return err },
			"watch Kustomizations": func() error {
				_, err := kustomizations.Watch(ctx, metav1.ListOptions{})
				return err
			},
			"patch a Kustomization": func() error {
				_, err := kustomizations.Patch(ctx, "apps-notes", "application/merge-patch+json", []byte(`{"spec":{"suspend":true}}`), metav1.PatchOptions{})
				return err
			},
			"create a Kustomization": func() error {
				_, err := kustomizations.Create(ctx, object("Kustomization"), metav1.CreateOptions{})
				return err
			},
			"list OCIRepositories": func() error { _, err := repositories.List(ctx, metav1.ListOptions{}); return err },
			"update an OCIRepository": func() error {
				_, err := repositories.Update(ctx, object("OCIRepository"), metav1.UpdateOptions{})
				return err
			},
			"list Canaries": func() error { _, err := canaries.List(ctx, metav1.ListOptions{}); return err },
			"patch a Canary": func() error {
				_, err := canaries.Patch(ctx, "notes-web", "application/merge-patch+json", []byte(`{"spec":{"skipAnalysis":true}}`), metav1.PatchOptions{})
				return err
			},
			"list Deployments": func() error { _, err := deployments.List(ctx, metav1.ListOptions{}); return err },
			"scale a Deployment": func() error {
				_, err := deployments.Patch(ctx, "notes-web", "application/merge-patch+json", []byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{})
				return err
			},
			"delete a Deployment": func() error { return deployments.Delete(ctx, "notes-web", metav1.DeleteOptions{}) },
			"list secrets": func() error {
				_, err := restricted.CoreV1().Secrets("flux-system").List(ctx, metav1.ListOptions{})
				return err
			},
			"list configmaps": func() error {
				_, err := restricted.CoreV1().ConfigMaps("notes-system").List(ctx, metav1.ListOptions{})
				return err
			},
			"list pods": func() error {
				_, err := restricted.CoreV1().Pods("notes-system").List(ctx, metav1.ListOptions{})
				return err
			},
			"read its own grant": func() error {
				_, err := restricted.RbacV1().ClusterRoles().Get(ctx, "delivery-reporter", metav1.GetOptions{})
				return err
			},
		}
		for name, do := range refused {
			if err := do(); !apierrors.IsForbidden(err) {
				t.Errorf("%s was not forbidden: %v", name, err)
			}
		}
	})
}
