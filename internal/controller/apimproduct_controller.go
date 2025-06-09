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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
	"github.com/hedinit/azure-apim-operator/internal/identity"
)

// APIMProductReconciler reconciles a APIMProduct object
type APIMProductReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimproducts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimproducts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimproducts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the APIMProduct object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *APIMProductReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var product apimv1.APIMProduct
	if err := r.Get(ctx, req.NamespacedName, &product); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("🧹 APIMProduct deleted, skipping", "name", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMProduct")
		return ctrl.Result{}, err
	}

	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		logger.Error(err, "❌ Failed to read operator namespace")
		return ctrl.Result{}, fmt.Errorf("read operator namespace: %w", err)
	}
	operatorNamespace := strings.TrimSpace(string(nsBytes))

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: product.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		logger.Error(err, "❌ Failed to get APIMService", "name", product.Spec.APIMService)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("🔗 Found APIMService", "name", apimService.Name)

	// 🔐 Fetch token from environment and identity helper
	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		return ctrl.Result{}, fmt.Errorf("missing AZURE_CLIENT_ID or AZURE_TENANT_ID")
	}

	token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		product.Status.Phase = "Error"
		product.Status.Message = "Failed to get Azure token"
		_ = r.Status().Update(ctx, &product)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 📦 Construct product config
	cfg := apim.APIMProductConfig{
		SubscriptionID: apimService.Spec.Subscription,
		ResourceGroup:  apimService.Spec.ResourceGroup,
		ServiceName:    product.Spec.APIMService,
		ProductID:      product.Spec.ProductID,
		DisplayName:    product.Spec.DisplayName,
		Description:    product.Spec.Description,
		Published:      product.Spec.Published,
		BearerToken:    token,
	}

	if err := apim.CreateProductIfNotExists(ctx, cfg); err != nil {
		logger.Error(err, "❌ Failed to create product in APIM", "productID", cfg.ProductID)
		product.Status.Phase = "Error"
		product.Status.Message = err.Error()
	} else {
		logger.Info("✅ Successfully created APIM product", "productID", cfg.ProductID)
		product.Status.Phase = "Created"
		product.Status.Message = "Product created successfully"
	}

	if err := r.Status().Update(ctx, &product); err != nil {
		logger.Error(err, "❌ Failed to update APIMProduct status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMProductReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMProduct{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return true },
			UpdateFunc:  func(e event.UpdateEvent) bool { return false },
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		Named("apimproduct").
		Complete(r)
}
