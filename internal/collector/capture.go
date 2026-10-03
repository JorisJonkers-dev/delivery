// Package collector captures the ClusterState snapshot: the two facts only the cluster can say,
// which node holds each bound volume and where each Process runs, written to the Estate
// repository when, and only when, they change
// (deploy-kit spec/v1/20-resolved-deployment.md#cluster-state).
package collector

import (
	"cmp"
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// The labels deploy-kit's render puts on every pod of a Process.
const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedBy      = "deploy-kit"
	processLabel   = "app.kubernetes.io/name"
	hostnameLabel  = "kubernetes.io/hostname"
)

// ManagedSelector selects the pods the model renders: a placement is where a Process runs, and
// only those pods are Processes.
const ManagedSelector = managedByLabel + "=" + managedBy

// Binding is a bound claim and the node holding its volume.
type Binding struct {
	Claim string
	Node  string
}

// Placement is a Process and a node it runs on.
type Placement struct {
	Process string
	Node    string
}

// Facts is what a snapshot records, without when it was captured: that is not a fact.
type Facts struct {
	Bindings   []Binding
	Placements []Placement
}

// Equal reports whether two captures say the same thing.
func (f Facts) Equal(other Facts) bool {
	return slices.Equal(f.Bindings, other.Bindings) && slices.Equal(f.Placements, other.Placements)
}

// Capture reads the facts out of what the cluster holds. The result is ordered and free of
// duplicates, so the same cluster always captures the same facts.
func Capture(volumes []corev1.PersistentVolume, claims []corev1.PersistentVolumeClaim, pods []corev1.Pod) Facts {
	held := make(map[string]string, len(volumes))
	for i := range volumes {
		if node, ok := nodeHolding(&volumes[i]); ok {
			held[volumes[i].Name] = node
		}
	}

	var facts Facts
	for i := range claims {
		claim := &claims[i]
		if claim.Status.Phase != corev1.ClaimBound {
			continue
		}
		if node, ok := held[claim.Spec.VolumeName]; ok {
			facts.Bindings = append(facts.Bindings, Binding{Claim: claim.Name, Node: node})
		}
	}
	for i := range pods {
		pod := &pods[i]
		process := pod.Labels[processLabel]
		if pod.Labels[managedByLabel] != managedBy || process == "" || pod.Spec.NodeName == "" || finished(pod) {
			continue
		}
		facts.Placements = append(facts.Placements, Placement{Process: process, Node: pod.Spec.NodeName})
	}

	facts.Bindings = ordered(facts.Bindings, func(a, b Binding) int {
		return cmp.Or(cmp.Compare(a.Claim, b.Claim), cmp.Compare(a.Node, b.Node))
	})
	facts.Placements = ordered(facts.Placements, func(a, b Placement) int {
		return cmp.Or(cmp.Compare(a.Process, b.Process), cmp.Compare(a.Node, b.Node))
	})
	return facts
}

// nodeHolding names the one node a volume lives on. A `local-path` volume says so in its node
// affinity; a volume any node can mount is held by none, and binds its claim to nothing.
func nodeHolding(volume *corev1.PersistentVolume) (string, bool) {
	affinity := volume.Spec.NodeAffinity
	if affinity == nil || affinity.Required == nil || len(affinity.Required.NodeSelectorTerms) != 1 {
		return "", false
	}
	for _, requirement := range affinity.Required.NodeSelectorTerms[0].MatchExpressions {
		if requirement.Key == hostnameLabel && requirement.Operator == corev1.NodeSelectorOpIn && len(requirement.Values) == 1 {
			return requirement.Values[0], true
		}
	}
	return "", false
}

// finished reports a pod that no longer runs anywhere: a completed Job's pod keeps its node name.
func finished(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func ordered[T comparable](items []T, compare func(a, b T) int) []T {
	slices.SortFunc(items, compare)
	return slices.Compact(items)
}
