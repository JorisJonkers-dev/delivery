package collector_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JorisJonkers-dev/delivery/internal/collector"
)

func volume(name string, nodes ...string) corev1.PersistentVolume {
	pv := corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if len(nodes) > 0 {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"enschede"}},
				{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: nodes},
			}}},
		}}
	}
	return pv
}

func claim(namespace, name, volumeName string, phase corev1.PersistentVolumeClaimPhase) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

func pod(name, process, node string, phase corev1.PodPhase, managed bool) corev1.Pod {
	labels := map[string]string{}
	if process != "" {
		labels["app.kubernetes.io/name"] = process
	}
	if managed {
		labels["app.kubernetes.io/managed-by"] = "deploy-kit"
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func TestCaptureRecordsWhereBoundVolumesSitAndWhereProcessesRun(t *testing.T) {
	facts := collector.Capture(
		[]corev1.PersistentVolume{
			volume("pv-postgres", "enschede-t1000-1"),
			volume("pv-notes", "frankfurt-1"),
			volume("pv-shared"),                    // any node can mount it
			volume("pv-two", "node-a", "node-b"),   // held by no one node
			volume("pv-unclaimed", "enschede-gtx"), // no claim binds it
		},
		[]corev1.PersistentVolumeClaim{
			claim("data", "postgres-data", "pv-postgres", corev1.ClaimBound),
			claim("notes", "notes-data", "pv-notes", corev1.ClaimBound),
			claim("notes", "shared", "pv-shared", corev1.ClaimBound),
			claim("notes", "two", "pv-two", corev1.ClaimBound),
			claim("notes", "pending", "", corev1.ClaimPending),
			claim("notes", "lost", "pv-notes", corev1.ClaimLost),
		},
		[]corev1.Pod{
			pod("postgres-0", "postgres", "enschede-t1000-1", corev1.PodRunning, true),
			pod("notes-b", "notes", "frankfurt-1", corev1.PodRunning, true),
			pod("notes-a", "notes", "enschede-gtx", corev1.PodPending, true),
			pod("notes-c", "notes", "frankfurt-1", corev1.PodRunning, true), // a second replica on the node
			pod("unscheduled", "notes", "", corev1.PodPending, true),
			pod("migration", "notes-migration", "frankfurt-1", corev1.PodSucceeded, true),
			pod("crashed", "notes-prepare", "frankfurt-1", corev1.PodFailed, true),
			pod("legacy", "legacy", "frankfurt-1", corev1.PodRunning, false),
			pod("nameless", "", "frankfurt-1", corev1.PodRunning, true),
		},
	)
	want := collector.Facts{
		Bindings: []collector.Binding{
			{Claim: "notes-data", Node: "frankfurt-1"},
			{Claim: "postgres-data", Node: "enschede-t1000-1"},
		},
		Placements: []collector.Placement{
			{Process: "notes", Node: "enschede-gtx"},
			{Process: "notes", Node: "frankfurt-1"},
			{Process: "postgres", Node: "enschede-t1000-1"},
		},
	}
	if !facts.Equal(want) {
		t.Fatalf("facts:\n%+v\nwant:\n%+v", facts, want)
	}
	if facts.Equal(collector.Facts{Bindings: want.Bindings}) || facts.Equal(collector.Facts{Placements: want.Placements}) {
		t.Fatal("facts that differ compared equal")
	}
}

func TestCaptureReadsAVolumeWithHalfAnAffinityAsHeldByNoNode(t *testing.T) {
	noTerms := volume("no-terms")
	noTerms.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{}
	twoTerms := volume("two-terms", "node-a")
	twoTerms.Spec.NodeAffinity.Required.NodeSelectorTerms = append(twoTerms.Spec.NodeAffinity.Required.NodeSelectorTerms, corev1.NodeSelectorTerm{})
	notIn := volume("not-in", "node-a")
	notIn.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[1].Operator = corev1.NodeSelectorOpNotIn

	facts := collector.Capture(
		[]corev1.PersistentVolume{noTerms, twoTerms, notIn},
		[]corev1.PersistentVolumeClaim{
			claim("n", "a", "no-terms", corev1.ClaimBound),
			claim("n", "b", "two-terms", corev1.ClaimBound),
			claim("n", "c", "not-in", corev1.ClaimBound),
		}, nil)
	if len(facts.Bindings) != 0 {
		t.Fatalf("bindings %+v", facts.Bindings)
	}
}

const empty = `apiVersion: state.jorisjonkers.dev/v1
kind: ClusterState
schemaVersion: 1.0.0
cluster: production
capturedAt: 2026-10-02T00:00:00Z
bindings: []
placements: []
`

var captured = time.Date(2026, 10, 3, 7, 0, 0, 0, time.FixedZone("CEST", 2*60*60))

func TestWriteIsADocumentReadReadsBack(t *testing.T) {
	facts := collector.Facts{
		Bindings:   []collector.Binding{{Claim: "postgres-data", Node: "enschede-t1000-1"}},
		Placements: []collector.Placement{{Process: "postgres", Node: "enschede-t1000-1"}},
	}
	document := string(collector.Write("production", captured, facts))
	want := `apiVersion: state.jorisjonkers.dev/v1
kind: ClusterState
schemaVersion: 1.0.0
cluster: "production"
capturedAt: "2026-10-03T05:00:00Z"
bindings:
  - { claim: "postgres-data", node: "enschede-t1000-1" }
placements:
  - { process: "postgres", node: "enschede-t1000-1" }
`
	if !strings.HasSuffix(document, "\n\n"+want) || !strings.HasPrefix(document, "# The ClusterState snapshot") {
		t.Fatalf("document:\n%s", document)
	}
	cluster, read, err := collector.Read([]byte(document))
	if err != nil || cluster != "production" || !read.Equal(facts) {
		t.Fatalf("read back %q %+v %v", cluster, read, err)
	}

	none := string(collector.Write("production", captured, collector.Facts{}))
	if !strings.HasSuffix(none, "bindings: []\nplacements: []\n") {
		t.Fatalf("an empty snapshot:\n%s", none)
	}
	if _, read, err := collector.Read([]byte(none)); err != nil || !read.Equal(collector.Facts{}) {
		t.Fatalf("read back %+v %v", read, err)
	}
}

func TestReadRefusesWhatIsNotASnapshot(t *testing.T) {
	cases := map[string]string{
		"not YAML":               "bindings: [",
		"a missing list":         strings.Replace(empty, "placements: []\n", "", 1),
		"a binding with no node": strings.Replace(empty, "bindings: []", "bindings: [{claim: a}]", 1),
		"another kind":           strings.Replace(empty, "kind: ClusterState", "kind: ConfigMap", 1),
	}
	for name, document := range cases {
		if _, _, err := collector.Read([]byte(document)); err == nil {
			t.Errorf("%s was read", name)
		}
	}
}

// cluster is three lists, or the failure to list one of them.
type cluster struct {
	volumes []corev1.PersistentVolume
	claims  []corev1.PersistentVolumeClaim
	pods    []corev1.Pod
	fail    string
}

var errList = errors.New("forbidden")

func (c cluster) Volumes(context.Context) ([]corev1.PersistentVolume, error) {
	if c.fail == "volumes" {
		return nil, errList
	}
	return c.volumes, nil
}

func (c cluster) Claims(context.Context) ([]corev1.PersistentVolumeClaim, error) {
	if c.fail == "claims" {
		return nil, errList
	}
	return c.claims, nil
}

func (c cluster) ManagedPods(context.Context) ([]corev1.Pod, error) {
	if c.fail == "pods" {
		return nil, errList
	}
	return c.pods, nil
}

// repository is one file, and the writes it took.
type repository struct {
	document string
	revision int
	commits  []string
	failRead bool
	failNext bool
}

func (r *repository) Snapshot(context.Context) ([]byte, string, error) {
	if r.failRead {
		return nil, "", errors.New("unreachable")
	}
	return []byte(r.document), "r" + strconv.Itoa(r.revision), nil
}

func (r *repository) Commit(_ context.Context, document []byte, revision, message string) error {
	if r.failNext || revision != "r"+strconv.Itoa(r.revision) {
		return errors.New("conflict")
	}
	r.document, r.revision = string(document), r.revision+1
	r.commits = append(r.commits, message)
	return nil
}

func newCollector(c collector.Cluster, r collector.Repository) collector.Collector {
	return collector.Collector{
		Name:       "production",
		Cluster:    c,
		Repository: r,
		Now:        func() time.Time { return captured },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRunCommitsAChangeOnceAndAnUnchangedClusterNever(t *testing.T) {
	live := cluster{
		volumes: []corev1.PersistentVolume{volume("pv-postgres", "enschede-t1000-1")},
		claims:  []corev1.PersistentVolumeClaim{claim("data", "postgres-data", "pv-postgres", corev1.ClaimBound)},
	}
	repo := &repository{document: empty}
	c := newCollector(live, repo)

	committed, err := c.Run(context.Background())
	if err != nil || !committed {
		t.Fatalf("first run: committed %v, error %v", committed, err)
	}
	if len(repo.commits) != 1 || repo.commits[0] != "chore: capture the ClusterState of production" {
		t.Fatalf("commits %v", repo.commits)
	}
	if !strings.Contains(repo.document, `capturedAt: "2026-10-03T05:00:00Z"`) || !strings.Contains(repo.document, `claim: "postgres-data"`) {
		t.Fatalf("document:\n%s", repo.document)
	}

	// A later run, at a later hour, of a cluster that says the same thing.
	c.Now = func() time.Time { return captured.Add(6 * time.Hour) }
	before := repo.document
	committed, err = c.Run(context.Background())
	if err != nil || committed {
		t.Fatalf("second run: committed %v, error %v", committed, err)
	}
	if len(repo.commits) != 1 || repo.document != before {
		t.Fatal("an unchanged cluster was committed again")
	}
}

func TestRunOnAnEmptyClusterLeavesTheEmptySnapshot(t *testing.T) {
	repo := &repository{document: empty}
	committed, err := newCollector(cluster{}, repo).Run(context.Background())
	if err != nil || committed || repo.document != empty {
		t.Fatalf("committed %v, error %v", committed, err)
	}
}

func TestRunFailsClosed(t *testing.T) {
	changed := cluster{pods: []corev1.Pod{pod("notes-a", "notes", "frankfurt-1", corev1.PodRunning, true)}}
	cases := map[string]struct {
		cluster cluster
		repo    *repository
		want    string
	}{
		"volumes cannot be listed":    {cluster{fail: "volumes"}, &repository{document: empty}, "list PersistentVolumes: forbidden"},
		"claims cannot be listed":     {cluster{fail: "claims"}, &repository{document: empty}, "list PersistentVolumeClaims: forbidden"},
		"pods cannot be listed":       {cluster{fail: "pods"}, &repository{document: empty}, "list pods: forbidden"},
		"the snapshot cannot be read": {changed, &repository{failRead: true}, "read the committed snapshot: unreachable"},
		"the snapshot is not one":     {changed, &repository{document: "kind: ConfigMap\n"}, "read the committed snapshot"},
		"the snapshot is of another cluster": {
			changed, &repository{document: strings.Replace(empty, "production", "staging", 1)},
			`the committed snapshot is of cluster "staging", and this is "production"`,
		},
		"the commit is refused": {changed, &repository{document: empty, failNext: true}, "commit the snapshot: conflict"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before := c.repo.document
			committed, err := newCollector(c.cluster, c.repo).Run(context.Background())
			if err == nil || committed || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("committed %v, error %v, want one naming %q", committed, err, c.want)
			}
			if c.repo.document != before {
				t.Fatal("the snapshot was written")
			}
		})
	}
}
