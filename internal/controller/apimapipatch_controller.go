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
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
	"github.com/hedinit/azure-apim-operator/internal/identity"
)

// APIMAPIPatchReconciler reconciles a APIMAPIPatch object
type APIMAPIPatchReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapipatches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapipatches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapipatches/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the APIMAPIPatch object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *APIMAPIPatchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("apimapipatch_controller")

	var patch apimv1.APIMAPIPatch
	if err := r.Get(ctx, req.NamespacedName, &patch); err != nil {
		logger.Error(err, "❌ Failed to get APIMAPIPatch")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: patch.Spec.APIID, Namespace: req.Namespace}, &apimApi); err != nil {
		logger.Error(err, "❌ Failed to get APIMAPI", "name", patch.Spec.APIID)
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

	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		return ctrl.Result{}, fmt.Errorf("missing AZURE_CLIENT_ID or AZURE_TENANT_ID")
	}

	token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		return ctrl.Result{}, err
	}

	config := apim.APIMDeploymentConfig{
		SubscriptionID: apimService.Spec.Subscription,
		ResourceGroup:  apimService.Spec.ResourceGroup,
		ServiceName:    apimService.Name,
		APIID:          patch.Spec.APIID,
		ServiceURL:     patch.Spec.ServiceURL,
		BearerToken:    token,
	}

	if err := apim.AssignServiceUrlToApi(ctx, config); err != nil {
		logger.Error(err, "🚫 Failed to patch service")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	logger.Info("✅ Service URL patched in APIM", "apiID", patch.Spec.APIID)

	// if err := apim.AssignProductToAPI(ctx, config); err != nil {
	// 	logger.Error(err, "❌ Failed to assign product")
	// 	return ctrl.Result{}, err
	// }

	logger.Info("✅ Successfully patched API", "apiID", config.APIID)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMAPIPatchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMAPIPatch{}).
		Named("apimapipatch").
		Complete(r)
}
