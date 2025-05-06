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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
)

// PodWatcherReconciler reconciles a PodWatcher object
type PodWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=podwatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=podwatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=podwatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PodWatcher object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *PodWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// logger := ctrl.Log.WithName("podwatcher_controller")

	// var pod corev1.Pod
	// if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
	// 	if client.IgnoreNotFound(err) == nil {
	// 		logger.Info("ℹ️ Pod no longer exists", "name", req.NamespacedName)
	// 		return ctrl.Result{}, nil
	// 	}
	// 	logger.Error(err, "❌ Failed to fetch Pod")
	// 	return ctrl.Result{}, err
	// }
	// logger.Info("✅ Successfully fetched Pod", "name", pod.Name)

	// labels := pod.GetLabels()
	// if labels["apim.hedinit.io/import"] != "true" {
	// 	logger.Info("ℹ️ Pod does not have 'apim.hedinit.io/import=true', skipping")
	// 	return ctrl.Result{}, nil
	// }
	// logger.Info("✅ Pod has 'import=true' label")

	// appName := labels["app"]
	// if appName == "" {
	// 	logger.Info("ℹ️ No 'app' label found on pod, skipping")
	// 	return ctrl.Result{}, nil
	// }
	// logger.Info("✅ Found app label", "app", appName)

	// // Find matching ingress
	// var ingressList netv1.IngressList
	// if err := r.List(ctx, &ingressList, client.InNamespace(pod.Namespace)); err != nil {
	// 	logger.Error(err, "❌ Unable to list ingresses")
	// 	return ctrl.Result{}, err
	// }
	// logger.Info("✅ Successfully listed ingresses", "count", len(ingressList.Items))

	// for _, ing := range ingressList.Items {
	// 	for _, rule := range ing.Spec.Rules {
	// 		for _, path := range rule.HTTP.Paths {
	// 			if path.Backend.Service != nil && path.Backend.Service.Name == appName {
	// 				host := rule.Host
	// 				swaggerPath := labels["apim.hedinit.io/swagger-path"]
	// 				if swaggerPath == "" {
	// 					swaggerPath = "/swagger/v1/swagger.json"
	// 				}

	// 				subscriptionID := labels["apim.hedinit.io/subscriptionid"]
	// 				resourceGroup := labels["apim.hedinit.io/resourcegroup"]
	// 				serviceName := labels["apim.hedinit.io/apim"]
	// 				revision := labels["apim.hedinit.io/revision"]
	// 				routePrefix := labels["apim.hedinit.io/routeprefix"]
	// 				if routePrefix == "" {
	// 					routePrefix = "/" + pod.Name
	// 				}

	// 				logger.Info("✅ Matched Ingress for app", "host", host)

	// 				apiObj := &apimv1.APIMAPI{
	// 					ObjectMeta: metav1.ObjectMeta{
	// 						Name:      ing.Name,
	// 						Namespace: pod.Namespace,
	// 						OwnerReferences: []metav1.OwnerReference{
	// 							*metav1.NewControllerRef(&pod, schema.GroupVersionKind{
	// 								Group:   "",
	// 								Version: "v1",
	// 								Kind:    "Pod",
	// 							}),
	// 						},
	// 					},
	// 					Spec: apimv1.APIMAPISpec{
	// 						Host:          host,
	// 						RoutePrefix:   routePrefix,
	// 						SwaggerPath:   swaggerPath,
	// 						APIMService:   serviceName,
	// 						Subscription:  subscriptionID,
	// 						ResourceGroup: resourceGroup,
	// 						Revision:      revision,
	// 					},
	// 				}

	// 				if err := r.Create(ctx, apiObj); err != nil {
	// 					logger.Error(err, "❌ Failed to create APIMAPI object")
	// 				} else {
	// 					logger.Info("📘 APIMAPI created from pod", "name", apiObj.Name)
	// 				}
	// 				return ctrl.Result{}, nil
	// 			}
	// 		}
	// 	}
	// }

	// logger.Info("ℹ️ No matching ingress found for pod")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Named("podwatcher").
		Complete(r)
}
