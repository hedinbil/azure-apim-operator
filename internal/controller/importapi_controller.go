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
	"strings"
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

// ImportAPIReconciler reconciles a ImportAPI object
type ImportAPIReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=apim.hedinit.io,resources=importapis,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apim.hedinit.io,resources=importapis/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apim.hedinit.io,resources=importapis/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ImportAPI object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *ImportAPIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("importapi_controller")

	var importApi apimv1.ImportAPI
	if err := r.Get(ctx, req.NamespacedName, &importApi); err != nil {
		logger.Error(err, "❌ Failed to get ImportAPI")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: importApi.Name, Namespace: req.Namespace}, &apimApi); err != nil {
		logger.Error(err, "❌ Failed to get APIMAPI", "name", importApi.Name)
		return ctrl.Result{}, err
	}

	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		logger.Error(err, "❌ Failed to read operator namespace")
		return ctrl.Result{}, err
	}
	operatorNamespace := strings.TrimSpace(string(nsBytes))

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: apimApi.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		logger.Error(err, "❌ Failed to get APIMService", "name", apimApi.Spec.APIMService)
		return ctrl.Result{}, err
	}

	// openApiURL := fmt.Sprintf("https://%s%s", apiRevision.Spec.Host, apiRevision.Spec.OpenAPIDefinitionURL)
	openApiURL := importApi.Spec.OpenAPIDefinitionURL
	logger.Info("📡 Fetching OpenAPI definition", "url", openApiURL, "name", importApi.Spec.APIID)

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

	config := apim.APIMDeploymentConfig{
		SubscriptionID: apimService.Spec.Subscription,
		ResourceGroup:  apimService.Spec.ResourceGroup,
		ServiceName:    apimService.Spec.Name,
		APIID:          importApi.Spec.APIID,
		RoutePrefix:    importApi.Spec.RoutePrefix,
		BearerToken:    token,
	}

	if err := apim.ImportOpenAPIDefinitionToAPIM(ctx, config, openApiContent); err != nil {
		logger.Error(err, "🚫 Failed to import API")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	logger.Info("✅ API imported to APIM", "apiID", importApi.Spec.APIID)

	// 🎯 Delete the ImportAPI CR once processed
	if err := r.Delete(ctx, &importApi); err != nil {
		logger.Error(err, "⚠️ Failed to delete ImportAPI object")
		return ctrl.Result{}, err
	}
	logger.Info("🧹 ImportAPI deleted after successful import", "name", importApi.Name)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImportAPIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.ImportAPI{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return true },
			UpdateFunc:  func(e event.UpdateEvent) bool { return false },
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		Named("importapi").
		Complete(r)
}
