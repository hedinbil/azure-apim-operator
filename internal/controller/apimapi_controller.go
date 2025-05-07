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

	apimv1 "github.com/hedinit/aks-apim-operator/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// APIMAPIReconciler reconciles a APIMAPI object
type APIMAPIReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis/finalizers,verbs=update

func (r *APIMAPIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	//logger := log.FromContext(ctx)
	// var logger = ctrl.Log.WithName("apimapi_controller")

	// var api apimv1.APIMAPI
	// if err := r.Get(ctx, req.NamespacedName, &api); err != nil {
	// 	logger.Error(err, "❌ Unable to fetch APIMAPI")
	// 	return ctrl.Result{}, client.IgnoreNotFound(err)
	// }

	// logger.Info("🔍 Fetched APIMAPI", "name", api.Name)

	// // Get bearer token (this assumes you already have a helper function)
	// clientID := os.Getenv("AZURE_CLIENT_ID")
	// tenantID := os.Getenv("AZURE_TENANT_ID")
	// if clientID == "" || tenantID == "" {
	// 	return ctrl.Result{}, fmt.Errorf("missing AZURE_CLIENT_ID or AZURE_TENANT_ID")
	// }

	// token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	// if err != nil {
	// 	logger.Error(err, "❌ Failed to get Azure token")
	// 	return ctrl.Result{}, err
	// }

	// config := apim.APIMRevisionConfig{
	// 	SubscriptionID: api.Spec.Subscription,
	// 	ResourceGroup:  api.Spec.ResourceGroup,
	// 	ServiceName:    api.Spec.APIMService,
	// 	APIID:          api.Name,
	// 	BearerToken:    token,
	// }

	// revisions, err := apim.GetAPIRevisions(ctx, config)
	// if err != nil {
	// 	logger.Error(err, "❌ Failed to fetch API revisions from APIM")
	// 	return ctrl.Result{}, err
	// }

	// for _, rev := range revisions {
	// 	logger.Info("📎 Found API revision",
	// 		"revision", rev.Properties.ApiRevision,
	// 		"isCurrent", rev.Properties.IsCurrent,
	// 		"name", rev.Name,
	// 	)
	// }

	return ctrl.Result{}, nil
}

func (r *APIMAPIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMAPI{}).
		Named("apimapi").
		Complete(r)
}
