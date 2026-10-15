package collector_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/JorisJonkers-dev/delivery/internal/collector"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp"
	"github.com/JorisJonkers-dev/delivery/internal/githubapp/githubtest"
)

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

	seed(t, admin)

	github := githubtest.New(t, empty)
	key, err := githubapp.ParseKey(github.PEM())
	if err != nil {
		t.Fatal(err)
	}
	now := captured
	c := collector.Collector{
		Name:    "production",
		Cluster: collector.Kube{Client: admin},
		Repository: githubapp.File{
			App: githubapp.App{
				API: github.URL, AppID: githubtest.AppID, InstallationID: githubtest.InstallationID, Key: key,
				HTTP: github.Client(), Now: time.Now,
			},
			Repository: githubtest.Repository, Path: githubtest.Path, Branch: githubtest.Branch,
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
