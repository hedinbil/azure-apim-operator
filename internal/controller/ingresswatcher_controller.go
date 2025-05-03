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
	"fmt"
	"net/http"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// IngressWatcherReconciler reconciles a IngressWatcher object
type IngressWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=net.hedinit.io,resources=ingresswatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=net.hedinit.io,resources=ingresswatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=net.hedinit.io,resources=ingresswatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the IngressWatcher object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *IngressWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var ingress networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		logger.Error(err, "unable to fetch Ingress")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Ingress found", "name", ingress.Name, "namespace", ingress.Namespace)

	val, ok := ingress.Annotations["hedinit.io/openapi-export"]
	if !ok || val != "true" {
		logger.Info("Annotation not present or not set to 'true', skipping")
		return ctrl.Result{}, nil
	}

	if len(ingress.Status.LoadBalancer.Ingress) == 0 {
		logger.Info("Ingress has no LoadBalancer status yet, will retry")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	host := ingress.Status.LoadBalancer.Ingress[0].Hostname
	swaggerURL := fmt.Sprintf("https://%s/swagger.yaml", host)

	logger.Info("Fetching Swagger YAML", "url", swaggerURL)

	resp, err := http.Get(swaggerURL)
	if err != nil {
		logger.Error(err, "Failed to fetch Swagger YAML")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer resp.Body.Close()

	logger.Info("Successfully fetched Swagger", "status", resp.StatusCode)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IngressWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Named("ingresswatcher").
		Complete(r)
}
