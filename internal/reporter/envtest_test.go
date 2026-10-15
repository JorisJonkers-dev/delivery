package reporter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
	"github.com/JorisJonkers-dev/delivery/internal/reporter"
)

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

	k := reporter.Kube{Client: admin, Dynamic: adminObjects, Namespace: "flux-system"}

	t.Run("it reads each unit, its pin and its Canaries", func(t *testing.T) {
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
}
