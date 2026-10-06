package controller

import (
	"testing"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestAPIMAPIDeploymentPredicateRetryAnnotation: apim.operator.io/retry is a
// metadata-only change with no generation bump, and must still reach Reconcile so a
// Stalled or Invalid deployment can write again.
func TestAPIMAPIDeploymentPredicateRetryAnnotation(t *testing.T) {
	base := &apimv1.APIMAPIDeployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", Generation: 3}}
	withAnnotations := func(annotations map[string]string) *apimv1.APIMAPIDeployment {
		obj := base.DeepCopy()
		obj.Annotations = annotations
		return obj
	}
	stalled := base.DeepCopy()
	stalled.Status.Phase = phaseStalled
	stalled.Status.ConsecutiveFailures = 5

	cases := []struct {
		name     string
		old, new *apimv1.APIMAPIDeployment
		want     bool
	}{
		{"annotation added", base, withAnnotations(map[string]string{retryAnnotation: "1"}), true},
		{"annotation value changed", withAnnotations(map[string]string{retryAnnotation: "1"}),
			withAnnotations(map[string]string{retryAnnotation: "2"}), true},
		{"annotation unchanged", withAnnotations(map[string]string{retryAnnotation: "1"}),
			withAnnotations(map[string]string{retryAnnotation: "1"}), false},
		{"annotation removed", withAnnotations(map[string]string{retryAnnotation: "1"}), base, true},
		{"unrelated annotation", base, withAnnotations(map[string]string{"example.com/note": "x"}), false},
		{"status moves to Stalled", base, stalled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apimAPIDeploymentPredicate().Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Errorf("Update() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAPIMAPIDeploymentUpdatePredicate(t *testing.T) {
	t.Run("ignores status-only updates", func(t *testing.T) {
		oldDeployment := &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "api",
				Namespace:  "default",
				Generation: 3,
			},
			Status: apimv1.APIMAPIDeploymentStatus{
				LastAttemptAt: "2026-01-01T00:00:00Z",
			},
		}

		newDeployment := oldDeployment.DeepCopy()
		newDeployment.Status.LastAttemptAt = "2026-01-01T00:00:05Z"

		if apimAPIDeploymentPredicate().Update(event.UpdateEvent{ObjectOld: oldDeployment, ObjectNew: newDeployment}) {
			t.Fatalf("expected status-only update to be ignored")
		}
	})

	t.Run("reconciles when generation changes", func(t *testing.T) {
		oldDeployment := &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "api",
				Namespace:  "default",
				Generation: 3,
			},
		}

		newDeployment := oldDeployment.DeepCopy()
		newDeployment.Generation = 4

		if !apimAPIDeploymentPredicate().Update(event.UpdateEvent{ObjectOld: oldDeployment, ObjectNew: newDeployment}) {
			t.Fatalf("expected generation change to trigger reconcile")
		}
	})

	t.Run("reconciles when signal annotation changes", func(t *testing.T) {
		oldDeployment := &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "api",
				Namespace:  "default",
				Generation: 3,
				Annotations: map[string]string{
					apimDeploymentSignalAnnotation: "2026-01-01T00:00:00Z",
				},
			},
		}

		newDeployment := oldDeployment.DeepCopy()
		newDeployment.Annotations[apimDeploymentSignalAnnotation] = "2026-01-01T00:00:05Z"

		if !apimAPIDeploymentPredicate().Update(event.UpdateEvent{ObjectOld: oldDeployment, ObjectNew: newDeployment}) {
			t.Fatalf("expected signal annotation update to trigger reconcile")
		}
	})
}
