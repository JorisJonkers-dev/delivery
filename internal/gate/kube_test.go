package gate_test

import (
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

const ns = "auth-system"

func labels(name, instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": name, "app.kubernetes.io/instance": instance, "app.kubernetes.io/part-of": "auth",
		"app.kubernetes.io/managed-by": "deploy-kit",
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
	kinds := map[schema.GroupVersionResource]string{
		gate.Canaries: "CanaryList",
		{Group: "secrets.hashicorp.com", Version: "v1beta1", Resource: "vaultauths"}:          "VaultAuthList",
		{Group: "secrets.hashicorp.com", Version: "v1beta1", Resource: "vaultstaticsecrets"}:  "VaultStaticSecretList",
		{Group: "secrets.hashicorp.com", Version: "v1beta1", Resource: "vaultdynamicsecrets"}: "VaultDynamicSecretList",
	}
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
	if _, err := k.Inputs(t.Context(), ns, "absent"); !errors.Is(err, gate.ErrNoInputs) {
		t.Fatalf("an Application with no ConfigMap has no inputs: %v", err)
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
		job("auth-migration-down-9d2c4e6a8b0d", "auth-migration", batchv1.JobCondition{Type: batchv1.JobSuspended, Status: yes}),
		job("auth-seed-9d2c4e6a8b0d", "auth-seed", batchv1.JobCondition{Type: batchv1.JobFailed, Status: yes}, batchv1.JobCondition{Type: batchv1.JobComplete, Status: no}),
		// A Job labelled as auth's that the render did not write is no release Job.
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "auth-migration-1111aaaa2222", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "auth", "app.kubernetes.io/name": "auth-migration"}}},
		// Another Application's Job in the same namespace is not this one's.
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "mail-migration-9d2c4e6a8b0d", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "mail"}}},
	})

	got, err := k.Jobs(t.Context(), ns, "auth")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]gate.Job{
		"auth-migration-9d2c4e6a8b0d":      {Name: "auth-migration-9d2c4e6a8b0d", Component: "auth-migration", Complete: true},
		"auth-migration-down-9d2c4e6a8b0d": {Name: "auth-migration-down-9d2c4e6a8b0d", Component: "auth-migration"},
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
			{Name: "both", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}}},
				{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: secret}}},
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
			}}}},
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
		"another projected ConfigMap": func(p *corev1.PodSpec) { p.Volumes[3].Projected.Sources[0].ConfigMap.Name = "settings-2" },
		"another projected Secret":    func(p *corev1.PodSpec) { p.Volumes[3].Projected.Sources[1].Secret.Name = "token-2" },
		// Anything the pod holds counts, not only what it runs.
		"another port":           func(p *corev1.PodSpec) { p.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 9090}} },
		"another ServiceAccount": func(p *corev1.PodSpec) { p.ServiceAccountName = "someone-else" },
		"a readiness probe":      func(p *corev1.PodSpec) { p.Containers[0].ReadinessProbe = &corev1.Probe{PeriodSeconds: 5} },
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

func TestAStopStartProcessHasStartedOnceEveryReplicaRunsWhatItsDeploymentHolds(t *testing.T) {
	two := int32(2)
	// The embedding model of the same Project: generation 3, observed, two of two updated and
	// available, and no pod of the version before it left.
	rolledOut := func() *appsv1.Deployment {
		d := deployment("auth-cache", corev1.Container{Name: "auth-cache", Image: "cache@sha256:aa"})
		d.Generation = 3
		d.Spec.Replicas = &two
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2}
		return d
	}
	started := func(d *appsv1.Deployment) (bool, error) {
		return kube([]runtime.Object{d}).Started(t.Context(), ns, "auth-cache")
	}
	if done, err := started(rolledOut()); err != nil || !done {
		t.Fatalf("a Deployment that rolled out = %v, %v", done, err)
	}
	unset := rolledOut()
	unset.Spec.Replicas = nil
	unset.Status = appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	if done, err := started(unset); err != nil || !done {
		t.Fatalf("an unset count is one: %v, %v", done, err)
	}
	ahead := rolledOut()
	ahead.Status.ObservedGeneration = 4
	if done, err := started(ahead); err != nil || !done {
		t.Fatalf("a status observed at a later generation still describes this spec: %v, %v", done, err)
	}

	for name, change := range map[string]func(*appsv1.DeploymentStatus){
		"its new spec not observed yet":  func(s *appsv1.DeploymentStatus) { s.ObservedGeneration = 2 },
		"a replica of the old version":   func(s *appsv1.DeploymentStatus) { s.UpdatedReplicas = 1 },
		"a replica not available yet":    func(s *appsv1.DeploymentStatus) { s.AvailableReplicas = 1 },
		"an old pod not gone yet":        func(s *appsv1.DeploymentStatus) { s.Replicas = 3 },
		"stopped, its new pod not there": func(s *appsv1.DeploymentStatus) { *s = appsv1.DeploymentStatus{ObservedGeneration: 3} },
	} {
		t.Run(name, func(t *testing.T) {
			d := rolledOut()
			change(&d.Status)
			if done, err := started(d); err != nil || done {
				t.Fatalf("a Deployment with %s reads as started: %v, %v", name, done, err)
			}
		})
	}
	if _, err := kube(nil).Started(t.Context(), ns, "auth-cache"); err == nil {
		t.Fatal("a Process with no Deployment has started nothing the gate can read")
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

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gate.Canaries: "CanaryList"})
	dyn.PrependReactor("get", "canaries", away)
	// A Canary the gate could not ask for is not a Canary that is not there.
	if _, err := (gate.Kube{Dynamic: dyn}).Canary(t.Context(), ns, "auth-api"); !errors.Is(err, errAway) || errors.Is(err, gate.ErrNoCanary) {
		t.Fatalf("canary: %v", err)
	}
	if _, err := kube(nil).Canary(t.Context(), ns, "absent"); !errors.Is(err, gate.ErrNoCanary) {
		t.Fatalf("a Canary that is not there: %v", err)
	}
	if _, err := k.Jobs(t.Context(), ns, "auth"); !errors.Is(err, errAway) {
		t.Fatalf("jobs: %v", err)
	}
	if _, err := k.NewCopy(t.Context(), ns, "auth-api"); !errors.Is(err, errAway) {
		t.Fatalf("pods: %v", err)
	}
}

func TestFlaggerIsThePodsOfTheDeliveryProjectsFlaggerProcess(t *testing.T) {
	at := func(name, namespace, instance string, change func(*corev1.Pod), ips ...string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/instance": instance}}}
		p.Spec.ServiceAccountName = "flagger"
		p.Status.Phase = corev1.PodRunning
		for _, ip := range ips {
			p.Status.PodIPs = append(p.Status.PodIPs, corev1.PodIP{IP: ip})
		}
		if change != nil {
			change(p)
		}
		return p
	}
	ending := metav1.Now()
	k := kube([]runtime.Object{
		at("flagger-1", "delivery-system", "flagger", nil, "10.42.0.7", "fd00::7"),
		at("flagger-2", "delivery-system", "flagger", nil, "10.42.1.9"),
		at("flagger-starting", "delivery-system", "flagger", nil),
		// The gate itself, and a pod that only calls itself Flagger somewhere else.
		at("release-gate-1", "delivery-system", "release-gate", nil, "10.42.0.8"),
		at("flagger-1", "auth-system", "flagger", nil, "10.42.3.3"),
		// A pod that no longer holds the address it lists: the cluster hands it to the next pod.
		at("flagger-ended", "delivery-system", "flagger", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }, "10.42.0.20"),
		at("flagger-failed", "delivery-system", "flagger", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }, "10.42.0.21"),
		at("flagger-pending", "delivery-system", "flagger", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }, "10.42.0.22"),
		at("flagger-ending", "delivery-system", "flagger", func(p *corev1.Pod) { p.DeletionTimestamp = &ending; p.Finalizers = []string{"keep"} }, "10.42.0.23"),
		// A pod on the host's network has its node's address, as every such pod there does.
		at("flagger-on-host", "delivery-system", "flagger", func(p *corev1.Pod) { p.Spec.HostNetwork = true }, "192.168.1.10"),
		// The label alone is not the identity.
		at("flagger-as-someone", "delivery-system", "flagger", func(p *corev1.Pod) { p.Spec.ServiceAccountName = "default" }, "10.42.0.24"),
	})

	got, err := k.Flagger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"10.42.0.7", "10.42.1.9", "fd00::7"}; !slices.Equal(got, want) {
		t.Fatalf("Flagger's addresses = %v, want %v", got, want)
	}

	client := fake.NewClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errAway })
	if _, err := (gate.Kube{Client: client}).Flagger(t.Context()); !errors.Is(err, errAway) {
		t.Fatalf("Flagger's pods, unreadable: %v", err)
	}
}

// synced is a sync object as the render writes it for an Application, kept past its release where
// it names a revision.
func synced(kind, name, application, revision string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion("secrets.hashicorp.com/v1beta1")
	o.SetKind(kind)
	o.SetNamespace(ns)
	o.SetName(name)
	o.SetUID(types.UID("uid-" + name))
	o.SetResourceVersion("7")
	o.SetLabels(map[string]string{"app.kubernetes.io/part-of": application, "app.kubernetes.io/managed-by": "deploy-kit"})
	if revision != "" {
		o.SetAnnotations(map[string]string{"estate.jorisjonkers.dev/revision": revision})
	}
	return o
}

func TestTheSyncObjectsAreTheApplicationsKeptVaultObjectsWithTheRevisionThatRenderedThem(t *testing.T) {
	k := kube(nil,
		synced("VaultAuth", "auth-api", "auth", revision),
		synced("VaultStaticSecret", "auth-api-database", "auth", earlier),
		synced("VaultDynamicSecret", "auth-api-creds", "auth", revision),
		// Pruned as usual, so not the gate's: it names no revision.
		synced("VaultStaticSecret", "auth-ui-config", "auth", ""),
		synced("VaultStaticSecret", "mail-api-database", "mail", earlier),
	)

	got, err := k.SyncObjects(t.Context(), ns, "auth")

	want := []gate.SyncObject{
		{Kind: "VaultAuth", Name: "auth-api", Revision: revision, UID: "uid-auth-api", Version: "7"},
		{Kind: "VaultDynamicSecret", Name: "auth-api-creds", Revision: revision, UID: "uid-auth-api-creds", Version: "7"},
		{Kind: "VaultStaticSecret", Name: "auth-api-database", Revision: earlier, UID: "uid-auth-api-database", Version: "7"},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("sync objects = %+v, %v", got, err)
	}
}

func TestASweepDeletesTheSyncObjectOnlyAsItWasRead(t *testing.T) {
	k := kube(nil, synced("VaultStaticSecret", "auth-api-database", "auth", earlier))
	fake := k.Dynamic.(*dynamicfake.FakeDynamicClient)
	var sent *metav1.Preconditions
	fake.PrependReactor("delete", "vaultstaticsecrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sent = action.(k8stesting.DeleteActionImpl).DeleteOptions.Preconditions
		return false, nil, nil
	})
	read := gate.SyncObject{Kind: "VaultStaticSecret", Name: "auth-api-database", Revision: earlier, UID: "uid-auth-api-database", Version: "7"}

	if err := k.Sweep(t.Context(), ns, read); err != nil {
		t.Fatal(err)
	}

	if sent == nil || *sent.UID != "uid-auth-api-database" || *sent.ResourceVersion != "7" {
		t.Fatalf("preconditions = %+v", sent)
	}
	if got, _ := k.SyncObjects(t.Context(), ns, "auth"); len(got) != 0 {
		t.Fatalf("left = %+v", got)
	}
}

func TestASweepOfAnObjectChangedOrGoneSinceItWasReadIsNoError(t *testing.T) {
	k := kube(nil, synced("VaultStaticSecret", "auth-api-database", "auth", earlier))
	read := gate.SyncObject{Kind: "VaultStaticSecret", Name: "auth-api-database", UID: "uid-auth-api-database", Version: "7"}
	fake := k.Dynamic.(*dynamicfake.FakeDynamicClient)
	fake.PrependReactor("delete", "vaultstaticsecrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "secrets.hashicorp.com", Resource: "vaultstaticsecrets"}, "auth-api-database", errors.New("rewritten"))
	})

	if err := k.Sweep(t.Context(), ns, read); err != nil {
		t.Fatalf("changed since: %v", err)
	}
	if err := kube(nil).Sweep(t.Context(), ns, read); err != nil {
		t.Fatalf("gone since: %v", err)
	}
	if err := kube(nil).Sweep(t.Context(), ns, gate.SyncObject{Kind: "Secret", Name: "x"}); err == nil {
		t.Fatal("swept a Secret")
	}
}
