package collector_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/JorisJonkers-dev/delivery/internal/collector"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
)

const rbacFile = "../../deploy/collector/rbac.yaml"

// applyRBAC creates exactly what deploy/collector/rbac.yaml declares, and returns the
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
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: o.Namespace}}
			if _, err := admin.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.CoreV1().ServiceAccounts(o.Namespace).Create(ctx, o, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			user = "system:serviceaccount:" + o.Namespace + ":" + o.Name
		case *rbacv1.ClusterRole:
			if _, err := admin.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		case *rbacv1.ClusterRoleBinding:
			if _, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("%s grants through a %T, which this test does not apply", rbacFile, object)
		}
	}
	if user == "" {
		t.Fatalf("%s declares no ServiceAccount", rbacFile)
	}
	return user
}

func TestTheCollectorInACluster(t *testing.T) {
	environment := &envtest.Environment{}
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
	user := applyRBAC(t, admin)

	// The Collector's own view of the cluster: the ServiceAccount of the manifest, and nothing more.
	asCollector := rest.CopyConfig(config)
	asCollector.Impersonate = rest.ImpersonationConfig{UserName: user}
	restricted, err := kubernetes.NewForConfig(asCollector)
	if err != nil {
		t.Fatal(err)
	}

	seed(t, admin)

	github := githubtest.New(t, empty)
	key, err := githubapp.ParseKey(github.PEM())
	if err != nil {
		t.Fatal(err)
	}
	now := captured
	c := collector.Collector{
		Name:    "production",
		Cluster: collector.Kube{Client: restricted},
		Repository: githubapp.File{
			API: github.URL, Repository: githubtest.Repository, Path: githubtest.Path, Branch: githubtest.Branch,
			AppID: githubtest.AppID, InstallationID: githubtest.InstallationID, Key: key,
			HTTP: github.Client(), Now: time.Now,
		},
		Now: func() time.Time { return now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	t.Run("it writes the snapshot the cluster says", func(t *testing.T) {
		committed, err := c.Run(ctx)
		if err != nil || !committed {
			t.Fatalf("committed %v, error %v", committed, err)
		}
		want := `apiVersion: state.jorisjonkers.dev/v1
kind: ClusterState
schemaVersion: 1.0.0
cluster: "production"
capturedAt: "2026-10-03T05:00:00Z"
bindings:
  - { claim: "postgres-data", node: "enschede-t1000-1" }
placements:
  - { process: "notes", node: "frankfurt-1" }
  - { process: "postgres", node: "enschede-t1000-1" }
`
		if got := github.Document(); !strings.HasSuffix(got, "\n\n"+want) {
			t.Fatalf("snapshot:\n%s\nwant it to end:\n%s", got, want)
		}
	})

	t.Run("an unchanged cluster commits nothing", func(t *testing.T) {
		now = captured.Add(6 * time.Hour)
		before := github.Document()
		committed, err := c.Run(ctx)
		if err != nil || committed {
			t.Fatalf("committed %v, error %v", committed, err)
		}
		if len(github.Commits()) != 1 || github.Document() != before {
			t.Fatalf("%d commits for one change", len(github.Commits()))
		}
	})

	t.Run("a Process that moved is one more commit", func(t *testing.T) {
		if err := admin.CoreV1().Pods("notes").Delete(ctx, "notes-a", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
			t.Fatal(err)
		}
		createPod(t, admin, "notes", "notes-b", "notes", "enschede-gtx", true)
		committed, err := c.Run(ctx)
		if err != nil || !committed {
			t.Fatalf("committed %v, error %v", committed, err)
		}
		got := github.Document()
		if len(github.Commits()) != 2 || !strings.Contains(got, `{ process: "notes", node: "enschede-gtx" }`) ||
			strings.Contains(got, "frankfurt-1") || !strings.Contains(got, `capturedAt: "2026-10-03T11:00:00Z"`) {
			t.Fatalf("after the move, %d commits and:\n%s", len(github.Commits()), got)
		}
	})

	t.Run("the ServiceAccount can do nothing beyond get and list", func(t *testing.T) {
		core := restricted.CoreV1()
		allowed := map[string]func() error{
			"get a pod": func() error { _, err := core.Pods("notes").Get(ctx, "notes-b", metav1.GetOptions{}); return err },
			"get a claim": func() error {
				_, err := core.PersistentVolumeClaims("data").Get(ctx, "postgres-data", metav1.GetOptions{})
				return err
			},
			"get a volume": func() error {
				_, err := core.PersistentVolumes().Get(ctx, "pv-postgres", metav1.GetOptions{})
				return err
			},
		}
		for name, do := range allowed {
			if err := do(); err != nil {
				t.Errorf("%s was refused: %v", name, err)
			}
		}

		zero := int64(0)
		refused := map[string]func() error{
			"watch pods": func() error { _, err := core.Pods("notes").Watch(ctx, metav1.ListOptions{}); return err },
			"create a pod": func() error {
				_, err := core.Pods("notes").Create(ctx, podObject("notes", "intruder", "notes", "frankfurt-1", true), metav1.CreateOptions{})
				return err
			},
			"update a pod": func() error {
				_, err := core.Pods("notes").Update(ctx, podObject("notes", "notes-b", "notes", "frankfurt-1", true), metav1.UpdateOptions{})
				return err
			},
			"patch a pod": func() error {
				_, err := core.Pods("notes").Patch(ctx, "notes-b", "application/merge-patch+json", []byte(`{"metadata":{"labels":{"x":"y"}}}`), metav1.PatchOptions{})
				return err
			},
			"delete a pod": func() error {
				return core.Pods("notes").Delete(ctx, "notes-b", metav1.DeleteOptions{GracePeriodSeconds: &zero})
			},
			"delete a claim": func() error {
				return core.PersistentVolumeClaims("data").Delete(ctx, "postgres-data", metav1.DeleteOptions{})
			},
			"delete a volume": func() error { return core.PersistentVolumes().Delete(ctx, "pv-postgres", metav1.DeleteOptions{}) },
			"read a pod's log": func() error {
				_, err := core.Pods("notes").GetLogs("notes-b", &corev1.PodLogOptions{}).DoRaw(ctx)
				return err
			},
			"list secrets":    func() error { _, err := core.Secrets("notes").List(ctx, metav1.ListOptions{}); return err },
			"list configmaps": func() error { _, err := core.ConfigMaps("notes").List(ctx, metav1.ListOptions{}); return err },
			"list nodes":      func() error { _, err := core.Nodes().List(ctx, metav1.ListOptions{}); return err },
			"list namespaces": func() error { _, err := core.Namespaces().List(ctx, metav1.ListOptions{}); return err },
			"list deployments": func() error {
				_, err := restricted.AppsV1().Deployments("notes").List(ctx, metav1.ListOptions{})
				return err
			},
			"read its own grant": func() error {
				_, err := restricted.RbacV1().ClusterRoles().Get(ctx, "delivery-collector", metav1.GetOptions{})
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

// seed fills the cluster with what the snapshot should record, and with what it should not.
func seed(t *testing.T, admin kubernetes.Interface) {
	t.Helper()
	ctx := context.Background()
	for _, namespace := range []string{"data", "notes"} {
		if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	createVolume(t, admin, "pv-postgres", "enschede-t1000-1")
	createVolume(t, admin, "pv-shared", "")
	createClaim(t, admin, "data", "postgres-data", "pv-postgres", corev1.ClaimBound)
	createClaim(t, admin, "notes", "shared", "pv-shared", corev1.ClaimBound)
	createClaim(t, admin, "notes", "waiting", "", corev1.ClaimPending)

	createPod(t, admin, "data", "postgres-0", "postgres", "enschede-t1000-1", true)
	createPod(t, admin, "notes", "notes-a", "notes", "frankfurt-1", true)
	createPod(t, admin, "notes", "legacy", "legacy", "frankfurt-1", false)
}

func createVolume(t *testing.T, admin kubernetes.Interface, name, node string) {
	t.Helper()
	pv := volume(name)
	if node != "" {
		pv = volume(name, node)
	}
	pv.Spec.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
	pv.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	pv.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/" + name}}
	if _, err := admin.CoreV1().PersistentVolumes().Create(context.Background(), &pv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func createClaim(t *testing.T, admin kubernetes.Interface, namespace, name, volumeName string, phase corev1.PersistentVolumeClaimPhase) {
	t.Helper()
	ctx := context.Background()
	pvc := claim(namespace, name, volumeName, "")
	pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
	created, err := admin.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, &pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// No controller runs here to bind it, so the test says what the binder would.
	created.Status.Phase = phase
	if _, err := admin.CoreV1().PersistentVolumeClaims(namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func podObject(namespace, name, process, node string, managed bool) *corev1.Pod {
	p := pod(name, process, node, "", managed)
	p.Namespace = namespace
	p.Spec.Containers = []corev1.Container{{Name: "main", Image: "example.invalid/main:1"}}
	return &p
}

func createPod(t *testing.T, admin kubernetes.Interface, namespace, name, process, node string, managed bool) {
	t.Helper()
	if _, err := admin.CoreV1().Pods(namespace).Create(context.Background(), podObject(namespace, name, process, node, managed), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}
