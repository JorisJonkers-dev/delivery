package reporter

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// The kinds the Reporter reads, as its api block in deploy/delivery.project.yml declares.
var (
	//nolint:gochecknoglobals // constants in all but type
	Kustomizations = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	//nolint:gochecknoglobals // constants in all but type
	OCIRepositories = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
)

// The pin source's annotations, which Flux copies into every event about the unit
// (deploy-kit spec/v1/55-delivery.md#what-a-pin-source-holds).
const (
	annotationRepository  = "event.toolkit.fluxcd.io/repository"
	annotationSHA         = "event.toolkit.fluxcd.io/sha"
	annotationEnvironment = "event.toolkit.fluxcd.io/environment"
	annotationVersion     = "event.toolkit.fluxcd.io/version"
	annotationURL         = "event.toolkit.fluxcd.io/url"
)

const (
	// Every unit a pin source applies carries this label, and so does nothing else of Flux's.
	managedBy = "app.kubernetes.io/managed-by=deploy-kit"
	// A Project's unit is named for it.
	unitPrefix = "apps-"
	// An inventory entry is <namespace>_<name>_<group>_<kind>.
	canaryGroup = "flagger.app"
	canaryKind  = "Canary"
)

// Kube is the Reporter's cluster. It reads four kinds and writes nothing: the units and their
// sources in Flux's namespace, and the Canaries and Deployments the units applied.
type Kube struct {
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
	// Namespace is Flux's, where the units live.
	Namespace string
}

var _ Cluster = Kube{}

// Units implements Cluster.
func (k Kube) Units(ctx context.Context) ([]Unit, error) {
	list, err := k.Dynamic.Resource(Kustomizations).Namespace(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedBy})
	if err != nil {
		return nil, err
	}
	var units []Unit
	for i := range list.Items {
		object := &list.Items[i]
		annotations := object.GetAnnotations()
		u := Unit{
			Name:    object.GetName(),
			Project: strings.TrimPrefix(object.GetName(), unitPrefix),
			Rollout: Rollout{
				Repository:  annotations[annotationRepository],
				SHA:         annotations[annotationSHA],
				Environment: annotations[annotationEnvironment],
				Version:     annotations[annotationVersion],
				URL:         annotations[annotationURL],
			},
		}
		if u.Rollout == (Rollout{}) {
			units = append(units, u)
			continue
		}
		if err := k.read(ctx, object, &u); err != nil {
			return nil, fmt.Errorf("read %s: %w", u.Name, err)
		}
		units = append(units, u)
	}
	return units, nil
}

// read fills in what Flux and Flagger say of a unit that carries a Rollout.
func (k Kube) read(ctx context.Context, object *unstructured.Unstructured, u *Unit) error {
	u.Applied, _, _ = unstructured.NestedString(object.Object, "status", "lastAppliedRevision")
	u.Attempted, _, _ = unstructured.NestedString(object.Object, "status", "lastAttemptedRevision")
	u.Ready = readyOf(object)

	source, _, _ := unstructured.NestedString(object.Object, "spec", "sourceRef", "name")
	repository, err := k.Dynamic.Resource(OCIRepositories).Namespace(k.Namespace).Get(ctx, source, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read its source: %w", err)
	}
	// Flux records an OCI revision as the digest it pulled, so the pin is that digest.
	u.Pinned, _, _ = unstructured.NestedString(repository.Object, "spec", "ref", "digest")

	entries, _, _ := unstructured.NestedSlice(object.Object, "status", "inventory", "entries")
	for _, entry := range entries {
		id, _, _ := unstructured.NestedString(entry.(map[string]any), "id")
		parts := strings.Split(id, "_")
		if len(parts) != 4 || parts[2] != canaryGroup || parts[3] != canaryKind {
			continue
		}
		member, err := k.member(ctx, parts[0], parts[1])
		if err != nil {
			return fmt.Errorf("read the Canary %s/%s: %w", parts[0], parts[1], err)
		}
		u.Members = append(u.Members, member)
	}
	return nil
}

func (k Kube) member(ctx context.Context, namespace, process string) (Member, error) {
	object, err := k.Dynamic.Resource(gate.Canaries).Namespace(namespace).Get(ctx, process, metav1.GetOptions{})
	if err != nil {
		return Member{}, err
	}
	m := Member{Process: process}
	m.Phase, _, _ = unstructured.NestedString(object.Object, "status", "phase")
	m.Applied, _, _ = unstructured.NestedString(object.Object, "status", "lastAppliedSpec")
	m.Promoted, _, _ = unstructured.NestedString(object.Object, "status", "lastPromotedSpec")
	iteration, _, _ := unstructured.NestedInt64(object.Object, "status", "iterations")
	iterations, _, _ := unstructured.NestedInt64(object.Object, "spec", "analysis", "iterations")
	m.Iteration, m.Iterations = int(iteration), int(iterations)
	// A time that does not parse is the zero time: a transition the Reporter cannot date is
	// one it cannot place in this release.
	transitioned, _, _ := unstructured.NestedString(object.Object, "status", "lastTransitionTime")
	m.Transitioned, _ = time.Parse(time.RFC3339, transitioned)
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, c := range conditions {
		fields, _ := c.(map[string]any)
		if kind, _, _ := unstructured.NestedString(fields, "type"); kind == "Promoted" {
			m.Message, _, _ = unstructured.NestedString(fields, "message")
		}
	}
	// Whether the primary serves the render only matters once Flagger says it is settled; the
	// Deployments are read only then.
	if Settled(m) {
		m.Serves, err = gate.Kube{Client: k.Client}.Serves(ctx, namespace, process)
		if err != nil {
			return Member{}, fmt.Errorf("compare it with its primary: %w", err)
		}
	}
	return m, nil
}

func readyOf(object *unstructured.Unstructured) Condition {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, c := range conditions {
		fields, _ := c.(map[string]any)
		if kind, _, _ := unstructured.NestedString(fields, "type"); kind != "Ready" {
			continue
		}
		var ready Condition
		ready.Status, _, _ = unstructured.NestedString(fields, "status")
		ready.Reason, _, _ = unstructured.NestedString(fields, "reason")
		ready.Message, _, _ = unstructured.NestedString(fields, "message")
		return ready
	}
	return Condition{}
}
