package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// Kube reads the cluster. It only ever gets and lists: the gate's ServiceAccount may read these
// five kinds and do nothing else.
type Kube struct {
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
}

var _ Cluster = Kube{}

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
	list, err := k.Client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelPartOf + "=" + application})
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
	return read, nil
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
