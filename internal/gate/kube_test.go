package gate_test

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

const ns = "auth-system"

func labels(name, instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": name, "app.kubernetes.io/instance": instance, "app.kubernetes.io/part-of": "auth",
	}
}

func pod(name string, l map[string]string, ready corev1.ConditionStatus, restarts ...int32) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: l}}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: ready}}
	for i, n := range restarts {
		status := corev1.ContainerStatus{RestartCount: n}
		if i == 0 {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, status)
			continue
		}
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, status)
	}
	return p
}

func job(name, component string, conditions ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels(component, name)},
		Status:     batchv1.JobStatus{Conditions: conditions},
	}
}

func canary(name string, webhooks []any, status map[string]any) *unstructured.Unstructured {
	object := map[string]any{
		"apiVersion": "flagger.app/v1beta1", "kind": "Canary",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"analysis": map[string]any{"webhooks": webhooks}},
	}
	if status != nil {
		object["status"] = status
	}
	return &unstructured.Unstructured{Object: object}
}

func webhook(name, application, revision string) any {
	metadata := map[string]any{"process": "auth-api"}
	if application != "" {
		metadata["application"] = application
	}
	if revision != "" {
		metadata["revision"] = revision
	}
	return map[string]any{"name": name, "type": "rollout", "url": "http://gate/" + name, "metadata": metadata}
}

// rendered is a Canary's three webhooks as the render writes them: one Application, one revision.
func rendered(at string) []any {
	return []any{webhook("may-start", "auth", at), webhook("checks", "auth", at), webhook("may-promote", "auth", at)}
}

func kube(objects []runtime.Object, canaries ...runtime.Object) gate.Kube {
	kinds := map[schema.GroupVersionResource]string{gate.Canaries: "CanaryList"}
	return gate.Kube{
		Client:  fake.NewClientset(objects...),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, canaries...),
	}
}

func TestTheInputsAreTheOneValueOfTheApplicationsConfigMap(t *testing.T) {
	k := kube([]runtime.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "auth-release-gate", Namespace: ns}, Data: map[string]string{"releaseGate.json": authInputs + "\n"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "empty-release-gate", Namespace: ns}, Data: map[string]string{"other": "x"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "auth-release-gate", Namespace: "elsewhere"}, Data: map[string]string{"releaseGate.json": "{}"}},
	})

	if got, err := k.Inputs(t.Context(), ns, "auth"); err != nil || got != authInputs+"\n" {
		t.Fatalf("inputs = %q, %v", got, err)
	}
	if _, err := k.Inputs(t.Context(), ns, "empty"); err == nil {
		t.Fatal("a ConfigMap without the key is no inputs")
	}
	if _, err := k.Inputs(t.Context(), ns, "absent"); err == nil {
		t.Fatal("an Application with no ConfigMap has no inputs")
	}
	// What the render writes is what the gate decides from, end to end.
	answer, err := gate.New(kube([]runtime.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "auth-release-gate", Namespace: ns}, Data: map[string]string{"releaseGate.json": authInputs + "\n"}},
	}, canary("auth-api", rendered(revision), nil))).MayStart(t.Context(), asks())
	if err != nil || !answer.Yes {
		t.Fatalf("may-start from the rendered ConfigMap = %+v, %v", answer, err)
	}
}

func TestAJobIsReadWithItsIdentityAndHowItEnded(t *testing.T) {
	yes, no := corev1.ConditionTrue, corev1.ConditionFalse
	k := kube([]runtime.Object{
		job("auth-migration-9d2c4e6a8b0d", "auth-migration", batchv1.JobCondition{Type: batchv1.JobComplete, Status: yes}),
		job("auth-migration-down-9d2c4e6a8b0d", "auth-migration-down", batchv1.JobCondition{Type: batchv1.JobSuspended, Status: yes}),
		job("auth-seed-9d2c4e6a8b0d", "auth-seed", batchv1.JobCondition{Type: batchv1.JobFailed, Status: yes}, batchv1.JobCondition{Type: batchv1.JobComplete, Status: no}),
		// Another Application's Job in the same namespace is not this one's.
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "mail-migration-9d2c4e6a8b0d", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "mail"}}},
	})

	got, err := k.Jobs(t.Context(), ns, "auth")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]gate.Job{
		"auth-migration-9d2c4e6a8b0d":      {Name: "auth-migration-9d2c4e6a8b0d", Component: "auth-migration", Complete: true},
		"auth-migration-down-9d2c4e6a8b0d": {Name: "auth-migration-down-9d2c4e6a8b0d", Component: "auth-migration-down"},
		"auth-seed-9d2c4e6a8b0d":           {Name: "auth-seed-9d2c4e6a8b0d", Component: "auth-seed", Failed: true},
	}
	if len(got) != len(want) {
		t.Fatalf("jobs = %+v", got)
	}
	for _, j := range got {
		if want[j.Name] != j {
			t.Fatalf("job %+v, want %+v", j, want[j.Name])
		}
	}
}

func TestACanaryIsReadWithWhatItsWebhooksCarryAndWhatFlaggerRecorded(t *testing.T) {
	k := kube(nil,
		canary("auth-api", rendered(revision),
			map[string]any{"phase": "WaitingPromotion", "lastAppliedSpec": "a2", "lastPromotedSpec": "a1", "iterations": int64(4)}),
		canary("auth-ui", rendered(revision), nil),
		// A Canary whose webhooks disagree, or carry too little, is not one the render wrote.
		canary("two-revisions", []any{webhook("may-start", "auth", revision), webhook("checks", "auth", earlier)}, nil),
		canary("two-applications", []any{webhook("may-start", "auth", revision), webhook("checks", "mail", revision)}, nil),
		canary("revision-on-one", []any{webhook("may-start", "auth", ""), webhook("checks", "auth", revision)}, nil),
		canary("no-revision", []any{webhook("may-start", "auth", "")}, nil),
		canary("no-application", []any{webhook("may-start", "", revision)}, nil),
		canary("not-webhooks", []any{"not a webhook"}, nil),
		canary("no-webhooks", nil, nil),
	)

	want := gate.Canary{Application: "auth", Revision: revision, Phase: "WaitingPromotion", Applied: "a2", Promoted: "a1", Iterations: 4}
	if got, err := k.Canary(t.Context(), ns, "auth-api"); err != nil || got != want {
		t.Fatalf("canary = %+v, %v", got, err)
	}
	if got, err := k.Canary(t.Context(), ns, "auth-ui"); err != nil || got != (gate.Canary{Application: "auth", Revision: revision}) {
		t.Fatalf("a Canary Flagger has not touched = %+v, %v", got, err)
	}
	for _, name := range []string{"two-revisions", "two-applications", "revision-on-one", "no-revision", "no-application", "not-webhooks", "no-webhooks", "absent"} {
		if got, err := k.Canary(t.Context(), ns, name); err == nil {
			t.Fatalf("%s: read as %+v, want an error", name, got)
		}
	}
}

func deployment(name string, containers ...corev1.Container) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	d.Spec.Template.Spec.Containers = containers
	return d
}

func TestAMemberServesItsRenderWhenItsPrimaryRunsWhatItsDeploymentHolds(t *testing.T) {
	env := func(configMap, secret string) []corev1.EnvVar {
		return []corev1.EnvVar{
			{Name: "MODE", Value: "lite"},
			{Name: "FROM_MAP", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}, Key: "k"}}},
			{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "k"}}},
			{Name: "POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		}
	}
	from := func(configMap, secret string) []corev1.EnvFromSource {
		return []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secret}}},
		}
	}
	volumes := func(configMap, secret string) []corev1.Volume {
		return []corev1.Volume{
			{Name: "settings", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}}}},
			{Name: "key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
	}
	// What the render holds for the member, and Flagger's copy of it: the same, under its own
	// names for the configuration objects it copied.
	target := func() *appsv1.Deployment {
		d := deployment("auth-api", corev1.Container{Name: "auth-api", Image: "auth-api@sha256:aa", Command: []string{"run"}, Args: []string{"--serve"}, Env: env("settings", "token"), EnvFrom: from("settings", "token")})
		d.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "wait", Image: "wait@sha256:bb"}}
		d.Spec.Template.Spec.Volumes = volumes("settings", "token")
		return d
	}
	primary := func() *appsv1.Deployment {
		d := deployment("auth-api-primary", corev1.Container{Name: "auth-api", Image: "auth-api@sha256:aa", Command: []string{"run"}, Args: []string{"--serve"}, Env: env("settings-primary", "token-primary"), EnvFrom: from("settings-primary", "token-primary")})
		d.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "wait", Image: "wait@sha256:bb"}}
		d.Spec.Template.Spec.Volumes = volumes("settings-primary", "token-primary")
		return d
	}

	serves := func(target, primary *appsv1.Deployment) (bool, error) {
		return kube([]runtime.Object{target, primary}).Serves(t.Context(), ns, "auth-api")
	}
	if same, err := serves(target(), primary()); err != nil || !same {
		t.Fatalf("a primary that runs what the Deployment holds = %v, %v", same, err)
	}
	changes := map[string]func(*corev1.PodSpec){
		"another image":      func(p *corev1.PodSpec) { p.Containers[0].Image = "auth-api@sha256:cc" },
		"another init image": func(p *corev1.PodSpec) { p.InitContainers[0].Image = "wait@sha256:cc" },
		"another command":    func(p *corev1.PodSpec) { p.Containers[0].Command = []string{"other"} },
		"another argument":   func(p *corev1.PodSpec) { p.Containers[0].Args = []string{"--other"} },
		"another value":      func(p *corev1.PodSpec) { p.Containers[0].Env[0].Value = "full" },
		"a variable more": func(p *corev1.PodSpec) {
			p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "NEW", Value: "1"})
		},
		"another ConfigMap key":     func(p *corev1.PodSpec) { p.Containers[0].Env[1].ValueFrom.ConfigMapKeyRef.Key = "other" },
		"another Secret":            func(p *corev1.PodSpec) { p.Containers[0].Env[2].ValueFrom.SecretKeyRef.Name = "rotated" },
		"another ConfigMap, whole":  func(p *corev1.PodSpec) { p.Containers[0].EnvFrom[0].ConfigMapRef.Name = "settings-2" },
		"another Secret, whole":     func(p *corev1.PodSpec) { p.Containers[0].EnvFrom[1].SecretRef.Name = "token-2" },
		"another mounted ConfigMap": func(p *corev1.PodSpec) { p.Volumes[0].ConfigMap.Name = "settings-2" },
		"another mounted Secret":    func(p *corev1.PodSpec) { p.Volumes[1].Secret.SecretName = "token-2" },
		"a container more": func(p *corev1.PodSpec) {
			p.Containers = append(p.Containers, corev1.Container{Name: "sidecar", Image: "s@sha256:dd"})
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := target()
			change(&changed.Spec.Template.Spec)
			if same, err := serves(changed, primary()); err != nil || same {
				t.Fatalf("a Deployment with %s reads as served by its primary: %v, %v", name, same, err)
			}
		})
	}
	if _, err := kube([]runtime.Object{target()}).Serves(t.Context(), ns, "auth-api"); err == nil {
		t.Fatal("a member with no primary serves nothing the gate can compare")
	}
	if _, err := kube([]runtime.Object{primary()}).Serves(t.Context(), ns, "auth-api"); err == nil {
		t.Fatal("a primary with no Deployment is no member")
	}
}

func TestTheNewCopyIsThePodsNamedForTheProcessNeverThePrimarys(t *testing.T) {
	k := kube([]runtime.Object{
		pod("auth-api-7c9d-x", labels("auth-api", "auth-api"), corev1.ConditionTrue, 0, 0),
		pod("auth-api-7c9d-y", labels("auth-api", "auth-api"), corev1.ConditionFalse, 1, 2),
		// Flagger's primary keeps the instance and takes its own name.
		pod("auth-api-primary-5b8f-z", labels("auth-api-primary", "auth-api"), corev1.ConditionTrue, 0, 9),
		pod("auth-ui-1a2b-w", labels("auth-ui", "auth-ui"), corev1.ConditionTrue),
	})

	got, err := k.NewCopy(t.Context(), ns, "auth-api")
	if err != nil || len(got) != 2 {
		t.Fatalf("new copy = %+v, %v", got, err)
	}
	want := map[string]gate.Pod{
		"auth-api-7c9d-x": {Name: "auth-api-7c9d-x", Ready: true},
		// An init container's restarts count with the containers'.
		"auth-api-7c9d-y": {Name: "auth-api-7c9d-y", Restarts: 3},
	}
	for _, p := range got {
		if want[p.Name] != p {
			t.Fatalf("pod %+v, want %+v", p, want[p.Name])
		}
	}
}

func TestAClusterThatDoesNotAnswerIsAnError(t *testing.T) {
	client := fake.NewClientset()
	away := func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errAway }
	client.PrependReactor("list", "jobs", away)
	client.PrependReactor("list", "pods", away)
	k := gate.Kube{Client: client}

	if _, err := k.Jobs(t.Context(), ns, "auth"); !errors.Is(err, errAway) {
		t.Fatalf("jobs: %v", err)
	}
	if _, err := k.NewCopy(t.Context(), ns, "auth-api"); !errors.Is(err, errAway) {
		t.Fatalf("pods: %v", err)
	}
}
