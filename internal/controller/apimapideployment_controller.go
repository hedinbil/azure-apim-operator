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

// APIMAPIDeploymentReconciler reconciles APIMAPIDeployment custom resources.
// This controller handles the complete workflow of deploying an API to Azure API Management:
// 1. Fetching the OpenAPI definition
// 2. Importing it into APIM
// 3. Configuring the service URL
// 4. Associating products and tags
// 5. Updating the APIMAPI status with host information
// 6. Cleaning up the deployment resource after successful completion
type APIMAPIDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments/finalizers,verbs=update

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

	// Fetch the APIMAPIDeployment resource that triggered this reconciliation.
	var deployment apimv1.APIMAPIDeployment
	if err := r.Get(ctx, req.NamespacedName, &deployment); err != nil {
		logger.Info("ℹ️ Unable to fetch APIMAPIDeployment")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("🧩 Loaded APIMAPIDeployment",
		"name", deployment.Name,
		"namespace", deployment.Namespace,
		"apiID", deployment.Spec.APIID,
		"revision", deployment.Spec.Revision,
		"routePrefix", deployment.Spec.RoutePrefix,
		"openApiUrl", deployment.Spec.OpenAPIDefinitionURL,
	)

	// Fetch the associated APIMAPI resource to update its status after deployment.
	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: deployment.Name, Namespace: req.Namespace}, &apimApi); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("ℹ️ APIMAPI not found, skipping revision creation", "name", deployment.Spec.APIID)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMAPI", "name", deployment.Spec.APIID)
		return ctrl.Result{}, err
	}
	logger.Info("🔗 Found APIMAPI for deployment", "apimapi", apimApi.Name, "status", apimApi.Status.Status)

	// Step 1: Fetch the OpenAPI definition from the specified URL.
	// This uses retry logic to handle transient network failures.
	openApiURL := deployment.Spec.OpenAPIDefinitionURL
	logger.Info("📡 Fetching OpenAPI definition", "url", openApiURL, "name", deployment.Spec.APIID)
	// resp, err := http.Get(openApiURL)
	// if err != nil {
	// 	logger.Error(err, "❌ Failed to fetch OpenAPI definition")
	// 	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	// }
	// defer resp.Body.Close()

	// openApiContent, err := io.ReadAll(resp.Body)
	// if err != nil {
	// 	logger.Error(err, "❌ Failed to read OpenAPI definition body")
	// 	return ctrl.Result{}, err
	// }

	// Fetch the OpenAPI definition with retry logic to handle transient failures.
	openApiContent, err := fetchOpenAPIDefinitionWithRetry(openApiURL, 5)
	if err != nil {
		logger.Error(err, "❌ Failed to fetch OpenAPI definition after retries")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("📥 OpenAPI definition downloaded",
		"bytes", len(openApiContent),
		"url", openApiURL,
		"apiID", deployment.Spec.APIID,
	)

	// Step 2: Acquire an Azure management token for authenticating with the APIM Management API.
	// The token is obtained using workload identity credentials.
	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		logger.Error(fmt.Errorf("missing identity env vars"), "❌ AZURE_CLIENT_ID or AZURE_TENANT_ID not set")
		return ctrl.Result{}, fmt.Errorf("missing AZURE_CLIENT_ID or AZURE_TENANT_ID")
	}
	token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	logger.Info("🔐 Obtained Azure AD token for APIM call", "apiID", deployment.Spec.APIID)

	// Step 3: Build the APIM deployment configuration with all necessary parameters.
	config := apim.APIMDeploymentConfig{
		SubscriptionID: deployment.Spec.Subscription,
		ResourceGroup:  deployment.Spec.ResourceGroup,
		ServiceName:    deployment.Spec.APIMService,
		APIID:          deployment.Spec.APIID,
		RoutePrefix:    deployment.Spec.RoutePrefix,
		ServiceURL:     deployment.Spec.ServiceURL,
		Revision:       deployment.Spec.Revision,
		BearerToken:    token,
		ProductIDs:     deployment.Spec.ProductIDs,
		TagIDs:         deployment.Spec.TagIDs,
	}
	logger.Info("🛠️ Built APIM deployment config",
		"apiID", config.APIID,
		"subscription", config.SubscriptionID,
		"resourceGroup", config.ResourceGroup,
		"serviceName", config.ServiceName,
		"routePrefix", config.RoutePrefix,
		"revision", config.Revision,
		"productCount", len(config.ProductIDs),
		"tagCount", len(config.TagIDs),
	)

	// Step 4: Import the OpenAPI definition into Azure APIM.
	// This creates or updates the API in APIM with the provided specification.
	if err := apim.ImportOpenAPIDefinitionToAPIM(ctx, config, openApiContent); err != nil {
		logger.Error(err, "🚫 Failed to import API")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("✅ API imported to APIM", "apiID", deployment.Spec.APIID)

	// Step 5: Update the backend service URL for the API.
	// This points the API to the correct backend service endpoint.
	if err := apim.AssignServiceUrlToApi(ctx, config); err != nil {
		logger.Error(err, "🚫 Failed to patch service URL")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("✅ Service URL patched in APIM", "apiID", deployment.Spec.APIID)

	// Step 6: Assign the API to all configured products (if any).
	// Products are used to group APIs and require subscriptions for access.
	if len(config.ProductIDs) > 0 {
		if err := apim.AssignProductsToAPI(ctx, config); err != nil {
			logger.Error(err, "🚫 Failed to assign API to products", "productIDs", config.ProductIDs)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		logger.Info("✅ API assigned to products", "apiID", config.APIID, "productIDs", config.ProductIDs)
	} else {
		logger.Info("ℹ️ No product IDs configured; skipping product assignment")
	}

	// Step 7: Assign the API to all configured tags (if any).
	// Tags help organize and categorize APIs for better management.
	if len(config.TagIDs) > 0 {
		if err := apim.AssignTagsToAPI(ctx, config); err != nil {
			logger.Error(err, "🚫 Failed to assign API to tags", "tagIDs", config.TagIDs)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		logger.Info("✅ API assigned to tags", "apiID", config.APIID, "tagIDs", config.TagIDs)
	} else {
		logger.Info("ℹ️ No tag IDs configured; skipping tag assignment")
	}

	// Step 8: Fetch APIM service host details and update the APIMAPI status.
	// This provides the full URLs for accessing the API through APIM.
	apiHost, developerPortalHost, err := apim.GetAPIMServiceDetails(ctx, config)
	if err != nil {
		logger.Error(err, "⚠️ Failed to fetch APIM details")
		return ctrl.Result{}, err
	}

	// Update the APIMAPI status with deployment information.
	apimApi.Status.ImportedAt = time.Now().Format(time.RFC3339)
	apimApi.Status.Status = "OK"
	apimApi.Status.ApiHost = fmt.Sprintf("https://%s%s", apiHost, deployment.Spec.RoutePrefix)
	apimApi.Status.DeveloperPortalHost = fmt.Sprintf("https://%s", developerPortalHost)

	if err := r.Status().Update(ctx, &apimApi); err != nil {
		logger.Error(err, "⚠️ Failed to update APIMAPI status")
		return ctrl.Result{}, err
	}
	logger.Info("📝 APIMAPI status updated after import",
		"name", apimApi.Name,
		"apiHost", apimApi.Status.ApiHost,
		"developerPortalHost", apimApi.Status.DeveloperPortalHost,
	)

	// Step 9: Clean up the deployment custom resource after successful completion.
	// The APIMAPIDeployment is a transient resource that triggers the deployment workflow.
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

// fetchOpenAPIDefinitionWithRetry fetches an OpenAPI definition from a URL with exponential backoff retry logic.
// It attempts to fetch the definition up to maxRetries times, with increasing delays between attempts
// (2s, 4s, 8s, 16s, 32s) to handle transient network failures or temporary service unavailability.
func fetchOpenAPIDefinitionWithRetry(url string, maxRetries int) ([]byte, error) {
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = fmt.Errorf("GET error: %w", err)
		} else {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()

			if readErr != nil {
				lastErr = fmt.Errorf("read body error: %w", readErr)
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if closeErr != nil {
					return nil, fmt.Errorf("close response body: %w", closeErr)
				}
				return body, nil
			} else {
				if closeErr != nil {
					lastErr = fmt.Errorf("unexpected status: %s\nbody: %s (close error: %v)", resp.Status, string(body), closeErr)
				} else {
					lastErr = fmt.Errorf("unexpected status: %s\nbody: %s", resp.Status, string(body))
				}
			}
		}

		// Exponential backoff: wait 2^attempt seconds before retrying.
		// This gives transient failures time to resolve while avoiding excessive retries.
		time.Sleep(time.Duration(2<<i) * time.Second) // 2s, 4s, 8s, 16s, 32s
	}

	return nil, fmt.Errorf("openapi fetch failed after %d attempts: %w", maxRetries, lastErr)
}
