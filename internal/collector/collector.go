package collector

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Cluster is what the Collector reads: three lists, and nothing else of the cluster.
type Cluster interface {
	Volumes(ctx context.Context) ([]corev1.PersistentVolume, error)
	Claims(ctx context.Context) ([]corev1.PersistentVolumeClaim, error)
	ManagedPods(ctx context.Context) ([]corev1.Pod, error)
}

// Repository is the one file the Collector writes, in the Estate repository.
type Repository interface {
	// Snapshot returns the committed snapshot and the revision it was read at.
	Snapshot(ctx context.Context) (document []byte, revision string, err error)
	// Commit replaces the snapshot read at revision, and fails if it changed since.
	Commit(ctx context.Context, document []byte, revision, message string) error
}

// Collector runs one capture.
type Collector struct {
	// Name is the cluster this Collector runs in, as the snapshot names it.
	Name       string
	Cluster    Cluster
	Repository Repository
	Now        func() time.Time
	Log        *slog.Logger
}

// Run captures the facts and commits them only when they differ from the committed snapshot.
// It reports whether it committed.
func (c Collector) Run(ctx context.Context) (bool, error) {
	facts, err := c.capture(ctx)
	if err != nil {
		return false, err
	}

	document, revision, err := c.Repository.Snapshot(ctx)
	if err != nil {
		return false, fmt.Errorf("read the committed snapshot: %w", err)
	}
	cluster, committed, err := Read(document)
	if err != nil {
		return false, err
	}
	// A snapshot of another cluster is not this Collector's to overwrite.
	if cluster != c.Name {
		return false, fmt.Errorf("the committed snapshot is of cluster %q, and this is %q", cluster, c.Name)
	}
	if facts.Equal(committed) {
		c.Log.InfoContext(ctx, "the cluster says what the snapshot says; nothing to commit",
			"bindings", len(facts.Bindings), "placements", len(facts.Placements))
		return false, nil
	}

	message := fmt.Sprintf("chore: capture the ClusterState of %s", c.Name)
	if err := c.Repository.Commit(ctx, Write(c.Name, c.Now(), facts), revision, message); err != nil {
		return false, fmt.Errorf("commit the snapshot: %w", err)
	}
	c.Log.InfoContext(ctx, "committed a changed snapshot",
		"bindings", len(facts.Bindings), "placements", len(facts.Placements))
	return true, nil
}

func (c Collector) capture(ctx context.Context) (Facts, error) {
	volumes, err := c.Cluster.Volumes(ctx)
	if err != nil {
		return Facts{}, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	claims, err := c.Cluster.Claims(ctx)
	if err != nil {
		return Facts{}, fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	pods, err := c.Cluster.ManagedPods(ctx)
	if err != nil {
		return Facts{}, fmt.Errorf("list pods: %w", err)
	}
	return Capture(volumes, claims, pods), nil
}
