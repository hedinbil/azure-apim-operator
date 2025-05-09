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

	apimv1 "github.com/hedinit/aks-apim-operator/api/v1"
	"github.com/hedinit/aks-apim-operator/internal/apim"
	"github.com/hedinit/aks-apim-operator/internal/identity"
)

// APIMAPIRevisionReconciler reconciles a APIMAPIRevision object
type APIMAPIRevisionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapirevisions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapirevisions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapirevisions/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the APIMAPIRevision object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *APIMAPIRevisionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	//logger := log.FromContext(ctx)
	var logger = ctrl.Log.WithName("apimapirevision_controller")

	var apiRevision apimv1.APIMAPIRevision
	if err := r.Get(ctx, req.NamespacedName, &apiRevision); err != nil {
		logger.Info("ℹ️ Unable to fetch APIMAPIRevision")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: apiRevision.Spec.APIID, Namespace: req.Namespace}, &apimApi); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("ℹ️ APIMAPI not found, skipping revision creation", "name", apiRevision.Spec.APIID)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMAPI", "name", apiRevision.Spec.APIID)
		return ctrl.Result{}, err
	}

	swaggerURL := fmt.Sprintf("https://%s%s", apiRevision.Spec.Host, apiRevision.Spec.SwaggerPath)
	logger.Info("📡 Fetching Swagger", "url", swaggerURL)

	resp, err := http.Get(swaggerURL)
	if err != nil {
		logger.Error(err, "❌ Failed to fetch Swagger")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	defer resp.Body.Close()

	swaggerYAML, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(err, "❌ Failed to read Swagger body")
		return ctrl.Result{}, err
	}

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

	config := apim.APIMRevisionConfig{
		SubscriptionID: apiRevision.Spec.Subscription,
		ResourceGroup:  apiRevision.Spec.ResourceGroup,
		ServiceName:    apiRevision.Spec.APIMService,
		APIID:          apiRevision.Spec.APIID,
		RoutePrefix:    apiRevision.Spec.RoutePrefix,
		ServiceURL:     fmt.Sprintf("https://%s", apiRevision.Spec.Host),
		BearerToken:    token,
		Revision:       apiRevision.Spec.Revision,
	}

	if err := apim.ImportSwaggerToAPIM(ctx, config, swaggerYAML); err != nil {
		logger.Error(err, "🚫 Failed to import API")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("✅ API imported to APIM", "apiID", apiRevision.Name)

	if err := apim.PatchServiceURL(ctx, config); err != nil {
		logger.Error(err, "🚫 Failed to patch service URL")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("✅ Service URL patched in APIM", "apiID", apiRevision.Name)

	// Get APIM details (hostnames)
	apiHost, developerPortalHost, err := apim.GetAPIMServiceDetails(ctx, config)
	if err != nil {
		logger.Error(err, "⚠️ Failed to fetch APIM details")
		return ctrl.Result{}, err
	}

	apimApi.Status.ImportedAt = time.Now().Format(time.RFC3339)
	apimApi.Status.SwaggerStatus = resp.Status
	apimApi.Status.ApiHost = apiHost
	apimApi.Status.DeveloperPortalHost = developerPortalHost

	if err := r.Status().Update(ctx, &apimApi); err != nil {
		logger.Error(err, "⚠️ Failed to update APIMAPI status")
		return ctrl.Result{}, err
	}

	// 🎯 Delete the APIMAPIRevision CR once processed
	if err := r.Delete(ctx, &apiRevision); err != nil {
		logger.Error(err, "⚠️ Failed to delete APIMAPIRevision object")
		return ctrl.Result{}, err
	}
	logger.Info("🧹 APIMAPIRevision deleted after successful import", "name", apiRevision.Name)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMAPIRevisionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMAPIRevision{}).
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
		Named("apimapirevision").
		Complete(r)
}
