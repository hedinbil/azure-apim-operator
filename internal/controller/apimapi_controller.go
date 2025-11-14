/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// APIMAPIReconciler reconciles APIMAPI custom resources.
// This controller manages the lifecycle of APIs in Azure API Management by updating
// annotations with API host information for integration with tools like ArgoCD.
// It only processes update events, not create or delete events.
type APIMAPIReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapis/finalizers,verbs=update

func (r *APIMAPIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// logger := log.FromContext(ctx)
	var logger = ctrl.Log.WithName("apimapi_controller")

	logger.Info("🔁 Reconciling APIMAPI", "name", req.Name, "namespace", req.Namespace)

	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, req.NamespacedName, &apimApi); err != nil {
		logger.Info("ℹ️ Unable to fetch APIMAPI")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("🔍 Fetched APIMAPI resource", "name", apimApi.Name)

	// Only update annotations if the API host is available and the annotation needs updating.
	// Use Patch instead of Update to avoid overwriting spec fields (like subscriptionRequired).
	expectedAnnotation := apimApi.Status.ApiHost
	currentAnnotation := ""
	if apimApi.Annotations != nil {
		currentAnnotation = apimApi.Annotations["link.argocd.argoproj.io/external-link"]
	}

	// Only patch if the annotation is missing or different
	if expectedAnnotation != "" && currentAnnotation != expectedAnnotation {
		// Use Patch to update only annotations without touching the spec
		patch := client.MergeFrom(apimApi.DeepCopy())
		if apimApi.Annotations == nil {
			apimApi.Annotations = make(map[string]string)
		}
		apimApi.Annotations["link.argocd.argoproj.io/external-link"] = expectedAnnotation

		if err := r.Patch(ctx, &apimApi, patch); err != nil {
			logger.Error(err, "❌ Failed to patch APIMAPI annotations")
			return ctrl.Result{}, err
		}
		logger.Info("✅ Successfully patched APIMAPI annotations", "name", apimApi.Name,
			"annotation", expectedAnnotation,
			"subscriptionRequired", apimApi.Spec.SubscriptionRequired)
	} else if expectedAnnotation == "" {
		logger.Info("ℹ️ Skipping annotation update - API host not available yet", "name", apimApi.Name)
	} else {
		logger.Info("ℹ️ Annotation already up to date", "name", apimApi.Name,
			"subscriptionRequired", apimApi.Spec.SubscriptionRequired)
	}

	logger.Info("✅ Successfully reconciled APIMAPI", "name", apimApi.Name)

	return ctrl.Result{}, nil
}

func (r *APIMAPIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMAPI{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return false
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		Named("apimapi").
		Complete(r)
}
