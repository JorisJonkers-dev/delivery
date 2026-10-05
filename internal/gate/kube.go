package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// The labels the render puts on every object of a Process (deploy-kit spec/v1/10-project-intent.md#the-label-set).
const (
	labelName     = "app.kubernetes.io/name"
	labelInstance = "app.kubernetes.io/instance"
	labelPartOf   = "app.kubernetes.io/part-of"
)

// Canaries is where Flagger's Canary objects live.
var Canaries = schema.GroupVersionResource{Group: "flagger.app", Version: "v1beta1", Resource: "canaries"} //nolint:gochecknoglobals // a constant in all but type

// Kube is the gate's cluster. It gets and lists five kinds, and writes two things: the release
// record, a ConfigMap of the gate's own in its own namespace, and `suspend` on a release Job of
// the render's that it starts
// (deploy-kit spec/v1/55-delivery.md#the-release-gate).
type Kube struct {
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
}

var _ Cluster = Kube{}

// Flagger is deployed by this repository's own project: the `flagger` Process of the `delivery`
// project (deploy/delivery.project.yml), whose namespace and labels the render derives.
const (
	flaggerNamespace = "delivery-system"
	flaggerInstance  = "flagger"
)

// Flagger implements Cluster: every address of every pod of the `flagger` Process that holds its
// address now. A pod that has ended, or is ending, still lists the address it had, and the
// cluster hands that address to the next pod: such a pod is not Flagger. Neither is one on the
// host's network, whose address is every such pod's on its node, nor one that runs as another
// identity than the Process's own.
func (k Kube) Flagger(ctx context.Context) ([]string, error) {
	list, err := k.Client.CoreV1().Pods(flaggerNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelInstance + "=" + flaggerInstance})
	if err != nil {
		return nil, err
	}
	var addresses []string
	for _, pod := range list.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || pod.Spec.HostNetwork || pod.Spec.ServiceAccountName != flaggerInstance {
			continue
		}
		for _, ip := range pod.Status.PodIPs {
			addresses = append(addresses, ip.IP)
		}
	}
	return addresses, nil
}

// Inputs implements Cluster.
func (k Kube) Inputs(ctx context.Context, namespace, application string) (string, error) {
	cm, err := k.Client.CoreV1().ConfigMaps(namespace).Get(ctx, InputsName(application), metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	value, held := cm.Data[InputsKey]
	if !held {
		return "", fmt.Errorf("gate: ConfigMap %s/%s holds no %s", namespace, cm.Name, InputsKey)
	}
	return value, nil
}

// Jobs implements Cluster.
func (k Kube) Jobs(ctx context.Context, namespace, application string) ([]Job, error) {
	// Only a Job the render wrote is a release Job: one anyone else labelled alike is not.
	selector := labelPartOf + "=" + application + "," + labelManagedBy + "=" + renderedBy
	list, err := k.Client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(list.Items))
	for _, job := range list.Items {
		jobs = append(jobs, Job{
			Name:      job.Name,
			Component: job.Labels[labelName],
			Complete:  holds(job.Status.Conditions, batchv1.JobComplete),
			Failed:    holds(job.Status.Conditions, batchv1.JobFailed),
			Suspended: job.Spec.Suspend != nil && *job.Spec.Suspend,
			Version:   job.ResourceVersion,
		})
	}
	return jobs, nil
}

func holds(conditions []batchv1.JobCondition, kind batchv1.JobConditionType) bool {
	for _, c := range conditions {
		if c.Type == kind && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

var errNotTheRenders = errors.New("gate: the Canary's webhooks do not carry one Application and one revision")

// Canary implements Cluster. The render gives every webhook of a Canary the same Application and
// revision; a Canary whose webhooks disagree, or carry neither, is not one the gate can read.
func (k Kube) Canary(ctx context.Context, namespace, process string) (Canary, error) {
	object, err := k.Dynamic.Resource(Canaries).Namespace(namespace).Get(ctx, process, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Canary{}, ErrNoCanary
	}
	if err != nil {
		return Canary{}, err
	}
	webhooks, _, _ := unstructured.NestedSlice(object.Object, "spec", "analysis", "webhooks")
	var read Canary
	for i, webhook := range webhooks {
		fields, _ := webhook.(map[string]any)
		application, _, _ := unstructured.NestedString(fields, "metadata", "application")
		revision, _, _ := unstructured.NestedString(fields, "metadata", "revision")
		if i == 0 {
			read.Application, read.Revision = application, revision
		}
		if application != read.Application || revision != read.Revision {
			return Canary{}, errNotTheRenders
		}
	}
	if read.Application == "" || read.Revision == "" {
		return Canary{}, errNotTheRenders
	}
	read.Phase, _, _ = unstructured.NestedString(object.Object, "status", "phase")
	read.Applied, _, _ = unstructured.NestedString(object.Object, "status", "lastAppliedSpec")
	read.Promoted, _, _ = unstructured.NestedString(object.Object, "status", "lastPromotedSpec")
	iterations, _, _ := unstructured.NestedInt64(object.Object, "status", "iterations")
	read.Iterations = int(iterations)
	// A time that does not parse is the zero time: a transition the gate cannot date is one it
	// cannot place after anything.
	transitioned, _, _ := unstructured.NestedString(object.Object, "status", "lastTransitionTime")
	read.Transitioned, _ = time.Parse(time.RFC3339, transitioned)
	return read, nil
}

// The render labels every object it writes (deploy-kit spec/v1/10-project-intent.md#the-label-set).
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	renderedBy     = "deploy-kit"
	recordedBy     = "release-gate"
)

// Applications implements Cluster: every ConfigMap the render wrote under the name an
// Application's release-gate inputs carry, in whatever namespace.
func (k Kube) Applications(ctx context.Context) ([]Gated, error) {
	list, err := k.Client.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + renderedBy})
	if err != nil {
		return nil, err
	}
	var gated []Gated
	for _, cm := range list.Items {
		application := cm.Labels[labelPartOf]
		if application != "" && cm.Name == InputsName(application) {
			gated = append(gated, Gated{Namespace: cm.Namespace, Application: application})
		}
	}
	return gated, nil
}

// RecordKey is the one key of an Application's release record.
const RecordKey = "record.json"

// RecordName is the name of an Application's release record: a ConfigMap the gate alone writes,
// named for the namespace and the Application both. A namespace is a DNS label and holds no
// dot, so the first dot says where it ends, and inputs put under one Application's id in another
// namespace name another record: they can neither claim this one first nor write over it.
func RecordName(namespace, application string) string {
	return namespace + "." + application + "-release-record"
}

// RecordNamespace is where every release record lives: the gate's own namespace, which the
// delivery project derives, so that no Application can write what the gate decides from.
const RecordNamespace = flaggerNamespace

var errAnotherNamespace = errors.New("gate: the release record names another namespace")

// Record implements Cluster. A record that names another namespace than the one asked about is
// not this Application's, and the gate cannot answer from it.
func (k Kube) Record(ctx context.Context, namespace, application string) (Record, error) {
	cm, err := k.Client.CoreV1().ConfigMaps(RecordNamespace).Get(ctx, RecordName(namespace, application), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Record{}, ErrNoRecord
	}
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal([]byte(cm.Data[RecordKey]), &record); err != nil {
		return Record{}, fmt.Errorf("gate: ConfigMap %s/%s does not parse: %w", RecordNamespace, cm.Name, err)
	}
	if record.Namespace != namespace {
		return Record{}, errAnotherNamespace
	}
	return record, nil
}

// SetRecord implements Cluster: the record is created the first time and replaced after.
func (k Kube) SetRecord(ctx context.Context, namespace, application string, record Record) error {
	record.Namespace = namespace
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: RecordName(namespace, application), Namespace: RecordNamespace,
			Labels: map[string]string{labelPartOf: application, labelManagedBy: recordedBy},
		},
		Data: map[string]string{RecordKey: string(encoded)},
	}
	configMaps := k.Client.CoreV1().ConfigMaps(RecordNamespace)
	existing, err := configMaps.Get(ctx, cm.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, cm, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	// A record is replaced only by one of the namespace it names, and only as read: a ConfigMap
	// planted under the record's name that names another namespace is not written over.
	var held Record
	if err := json.Unmarshal([]byte(existing.Data[RecordKey]), &held); err != nil || held.Namespace != namespace {
		return errAnotherNamespace
	}
	cm.ResourceVersion = existing.ResourceVersion
	_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// Start implements Cluster. The patch names the version of the Job the gate read, so a Job put
// in its place since, under the same name, is not the one started.
func (k Kube) Start(ctx context.Context, namespace string, job Job) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": job.Version},
		"spec":     map[string]any{"suspend": false},
	})
	if err != nil {
		return err
	}
	_, err = k.Client.BatchV1().Jobs(namespace).Patch(ctx, job.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// primarySuffix is what Flagger adds to the name of a member's primary, and to the name of every
// ConfigMap and Secret it copies for it.
const primarySuffix = "-primary"

// Serves implements Cluster: the member's Deployment and its primary run the same thing.
func (k Kube) Serves(ctx context.Context, namespace, process string) (bool, error) {
	deployments := k.Client.AppsV1().Deployments(namespace)
	target, err := deployments.Get(ctx, process, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	primary, err := deployments.Get(ctx, process+primarySuffix, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return equality.Semantic.DeepEqual(target.Spec.Template.Spec, uncopied(primary.Spec.Template.Spec)), nil
}

// uncopied is a primary's pod with the names Flagger gave its copies of the configuration put
// back: every ConfigMap and Secret a volume, a variable or a container's environment names.
// What is left differs from the member's own pod only where the member changed.
func uncopied(primary corev1.PodSpec) corev1.PodSpec {
	pod := *primary.DeepCopy()
	source := func(name *string) { *name = strings.TrimSuffix(*name, primarySuffix) }
	for i := range pod.Volumes {
		uncopyVolume(&pod.Volumes[i], source)
	}
	for _, containers := range [][]corev1.Container{pod.InitContainers, pod.Containers} {
		for i := range containers {
			uncopyEnvironment(&containers[i], source)
		}
	}
	return pod
}

func uncopyVolume(v *corev1.Volume, source func(*string)) {
	if v.ConfigMap != nil {
		source(&v.ConfigMap.Name)
	}
	if v.Secret != nil {
		source(&v.Secret.SecretName)
	}
	if v.Projected == nil {
		return
	}
	for i := range v.Projected.Sources {
		if p := &v.Projected.Sources[i]; p.ConfigMap != nil {
			source(&p.ConfigMap.Name)
		} else if p.Secret != nil {
			source(&p.Secret.Name)
		}
	}
}

func uncopyEnvironment(c *corev1.Container, source func(*string)) {
	for i := range c.Env {
		from := c.Env[i].ValueFrom
		switch {
		case from == nil:
		case from.ConfigMapKeyRef != nil:
			source(&from.ConfigMapKeyRef.Name)
		case from.SecretKeyRef != nil:
			source(&from.SecretKeyRef.Name)
		}
	}
	for i := range c.EnvFrom {
		if f := &c.EnvFrom[i]; f.ConfigMapRef != nil {
			source(&f.ConfigMapRef.Name)
		} else if f.SecretRef != nil {
			source(&f.SecretRef.Name)
		}
	}
}

// NewCopy implements Cluster. Flagger's primary carries the member's `instance` and its own
// `name`, `<process>-primary`, so the two labels together select the new copy alone.
func (k Kube) NewCopy(ctx context.Context, namespace, process string) ([]Pod, error) {
	selector := labelInstance + "=" + process + "," + labelName + "=" + process
	list, err := k.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	pods := make([]Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		var restarts int32
		for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			restarts += status.RestartCount
		}
		ready := false
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		pods = append(pods, Pod{Name: pod.Name, Ready: ready, Restarts: restarts})
	}
	return pods, nil
}
