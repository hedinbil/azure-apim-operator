package main

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStripPodKeepsWhatTheControllersRead is the APIM-15 regression. The
// deployment controller lists every Pod in a namespace through the cached
// client, which starts a cluster-wide Pod informer; the untrimmed objects did
// not fit the operator's memory limit on a large cluster. The transform must
// keep exactly the fields the controllers read and drop the rest.
func TestStripPodKeepsWhatTheControllersRead(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orders-abc123",
			Namespace: "orders-prod",
			Labels:    map[string]string{"app.kubernetes.io/name": "orders"},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "orders-abc", Controller: ptr(true)},
			},
			Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{...}"},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "orders",
			Containers: []corev1.Container{{
				Name:  "orders",
				Image: "orders:1",
				Env:   []corev1.EnvVar{{Name: "BIG", Value: "payload"}},
			}},
			Volumes: []corev1.Volume{{Name: "config"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "orders", RestartCount: 3}},
			HostIP:            "10.0.0.1",
		},
	}
	out, err := stripPod(pod)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.(*corev1.Pod)
	if !ok {
		t.Fatalf("stripPod returned %T", out)
	}

	// Kept: identity, labels, owner references, phase and conditions.
	if got.Name != "orders-abc123" || got.Namespace != "orders-prod" {
		t.Errorf("identity lost: %s/%s", got.Namespace, got.Name)
	}
	if got.Labels["app.kubernetes.io/name"] != "orders" {
		t.Errorf("labels lost: %v", got.Labels)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "orders-abc" {
		t.Errorf("owner references lost: %v", got.OwnerReferences)
	}
	if got.Status.Phase != corev1.PodRunning {
		t.Errorf("phase lost: %s", got.Status.Phase)
	}
	if len(got.Status.Conditions) != 1 || got.Status.Conditions[0].Type != corev1.PodReady {
		t.Errorf("conditions lost: %v", got.Status.Conditions)
	}

	// Dropped: the bulk nobody reads.
	if len(got.Spec.Containers) != 0 || got.Spec.ServiceAccountName != "" || len(got.Spec.Volumes) != 0 {
		t.Errorf("pod spec was kept: %+v", got.Spec)
	}
	if len(got.Status.ContainerStatuses) != 0 || got.Status.HostIP != "" {
		t.Errorf("unused status was kept: %+v", got.Status)
	}
	if got.Annotations != nil {
		t.Errorf("annotations were kept: %v", got.Annotations)
	}
	if got.ManagedFields != nil {
		t.Errorf("managed fields were kept: %v", got.ManagedFields)
	}
}

func TestStripReplicaSetKeepsWhatTheControllersRead(t *testing.T) {
	three := int32(3)
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "orders-abc",
			Namespace:     "orders-prod",
			Labels:        map[string]string{"app.kubernetes.io/name": "orders"},
			Annotations:   map[string]string{"deployment.kubernetes.io/revision": "7"},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager"}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &three,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "orders", Image: "orders:1"}}},
			},
		},
		Status: appsv1.ReplicaSetStatus{ReadyReplicas: 2, Replicas: 3, ObservedGeneration: 7},
	}
	out, err := stripReplicaSet(replicaSet)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.(*appsv1.ReplicaSet)
	if !ok {
		t.Fatalf("stripReplicaSet returned %T", out)
	}

	if got.Name != "orders-abc" || got.Labels["app.kubernetes.io/name"] != "orders" {
		t.Errorf("identity or labels lost: %s %v", got.Name, got.Labels)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
		t.Errorf("desired replicas lost: %v", got.Spec.Replicas)
	}
	if got.Status.ReadyReplicas != 2 {
		t.Errorf("ready replicas lost: %d", got.Status.ReadyReplicas)
	}
	// The pod template is a whole pod spec again and is never read.
	if len(got.Spec.Template.Spec.Containers) != 0 {
		t.Errorf("pod template was kept: %+v", got.Spec.Template)
	}
	if got.Annotations != nil || got.ManagedFields != nil {
		t.Errorf("annotations or managed fields were kept")
	}
}

// TestTransformsPassThroughOtherTypes: a transform is registered per type, but
// it must not panic or corrupt anything if handed something else.
func TestTransformsPassThroughOtherTypes(t *testing.T) {
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}}
	for name, transform := range map[string]func(any) (any, error){
		"stripPod":        stripPod,
		"stripReplicaSet": stripReplicaSet,
	} {
		out, err := transform(other)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if out != any(other) {
			t.Errorf("%s did not pass the object through unchanged", name)
		}
	}
}

// TestCacheOptionsCoverBothCachedWorkloadTypes keeps the registration honest:
// the transforms only help if they are actually wired into the manager.
func TestCacheOptionsCoverBothCachedWorkloadTypes(t *testing.T) {
	opts := cacheOptions()
	if len(opts.ByObject) != 2 {
		t.Fatalf("expected transforms for Pod and ReplicaSet, got %d entries", len(opts.ByObject))
	}
	seen := map[string]bool{}
	for key, byObject := range opts.ByObject {
		if byObject.Transform == nil {
			t.Errorf("%T is registered without a transform", key)
			continue
		}
		seen[fmt.Sprintf("%T", key)] = true
	}
	for _, want := range []string{"*v1.Pod", "*v1.ReplicaSet"} {
		if !seen[want] {
			t.Errorf("no transform registered for %s, got %v", want, seen)
		}
	}
}

func ptr[T any](v T) *T { return &v }
