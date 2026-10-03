package collector

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Kube reads the three lists from a cluster. It only ever lists: the Collector's ServiceAccount
// may get and list these three kinds and do nothing else.
type Kube struct {
	Client kubernetes.Interface
}

func (k Kube) Volumes(ctx context.Context) ([]corev1.PersistentVolume, error) {
	list, err := k.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k Kube) Claims(ctx context.Context) ([]corev1.PersistentVolumeClaim, error) {
	list, err := k.Client.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k Kube) ManagedPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := k.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: ManagedSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
