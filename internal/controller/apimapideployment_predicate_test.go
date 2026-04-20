package controller

import (
	"testing"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

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
