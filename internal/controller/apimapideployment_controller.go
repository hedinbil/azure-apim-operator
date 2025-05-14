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
	"io"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
	"github.com/hedinit/azure-apim-operator/internal/identity"
)

// APIMAPIDeploymentReconciler reconciles a APIMAPIDeployment object
type APIMAPIDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapideployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapideployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapideployments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the APIMAPIDeployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *APIMAPIDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("apimapideployment_controller")

	var deployment apimv1.APIMAPIDeployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		logger.Info("ℹ️ Unable to fetch APIMAPIDeployment")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: deployment.Name, Namespace: req.Namespace}, &apimApi); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("ℹ️ APIMAPI not found, skipping revision creation", "name", deployment.Spec.APIID)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMAPI", "name", deployment.Spec.APIID)
		return ctrl.Result{}, err
	}

	// 1) Fetch the OpenAPI definition
	openApiURL := deployment.Spec.OpenAPIDefinitionURL
	logger.Info("📡 Fetching OpenAPI definition", "url", openApiURL, "name", deployment.Spec.APIID)
	resp, err := http.Get(openApiURL)
	if err != nil {
		logger.Error(err, "❌ Failed to fetch OpenAPI definition")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	defer resp.Body.Close()

	openApiContent, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(err, "❌ Failed to read OpenAPI definition body")
		return ctrl.Result{}, err
	}

	// 2) Acquire an Azure management token
	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		return ctrl.Result{}, fmt.Errorf("missing AZURE_CLIENT_ID or AZURE_TENANT_ID")
	}
	token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 3) Build our APIMDeploymentConfig, including ProductID
	config := apim.APIMDeploymentConfig{
		SubscriptionID: deployment.Spec.Subscription,
		ResourceGroup:  deployment.Spec.ResourceGroup,
		ServiceName:    deployment.Spec.APIMService,
		APIID:          deployment.Spec.APIID,
		RoutePrefix:    deployment.Spec.RoutePrefix,
		ServiceURL:     deployment.Spec.ServiceURL,
		Revision:       deployment.Spec.Revision,
		BearerToken:    token,
		ProductID:      deployment.Spec.ProductID,
	}

	// 4) Import the API
	if err := apim.ImportOpenAPIDefinitionToAPIM(ctx, config, openApiContent); err != nil {
		logger.Error(err, "🚫 Failed to import API")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("✅ API imported to APIM", "apiID", deployment.Spec.APIID)

	// 5) Patch the backend service URL
	if err := apim.AssignServiceUrlToApi(ctx, config); err != nil {
		logger.Error(err, "🚫 Failed to patch service URL")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("✅ Service URL patched in APIM", "apiID", deployment.Spec.APIID)

	// 6) Assign the API to the Product if set
	// if err := apim.AssignProductToAPI(ctx, config); err != nil {
	// 	logger.Error(err, "🚫 Failed to assign API to product", "productID", config.ProductID)
	// 	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	// }
	// logger.Info("✅ API assigned to product", "apiID", config.APIID, "productID", config.ProductID)

	// 7) Fetch APIM host details and update status
	apiHost, developerPortalHost, err := apim.GetAPIMServiceDetails(ctx, config)
	if err != nil {
		logger.Error(err, "⚠️ Failed to fetch APIM details")
		return ctrl.Result{}, err
	}

	apimApi.Status.ImportedAt = time.Now().Format(time.RFC3339)
	apimApi.Status.Status = resp.Status
	apimApi.Status.ApiHost = fmt.Sprintf("https://%s%s", apiHost, deployment.Spec.RoutePrefix)
	apimApi.Status.DeveloperPortalHost = fmt.Sprintf("https://%s", developerPortalHost)

	if err := r.Status().Update(ctx, &apimApi); err != nil {
		logger.Error(err, "⚠️ Failed to update APIMAPI status")
		return ctrl.Result{}, err
	}

	// 8) Clean up the deployment CR
	if err := r.Delete(ctx, &deployment); err != nil {
		logger.Error(err, "⚠️ Failed to delete APIMAPIDeployment object")
		return ctrl.Result{}, err
	}
	logger.Info("🧹 APIMAPIDeployment deleted after successful import", "name", deployment.Name)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMAPIDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMAPIDeployment{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		Named("apimapideployment").
		Complete(r)
}
