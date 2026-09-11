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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
	"github.com/hedinit/azure-apim-operator/internal/identity"
)

// APIMProductReconciler reconciles APIMProduct custom resources.
// This controller manages products in Azure API Management, which are used to group
// APIs and require subscriptions for access. Products can be published or unpublished
// to control visibility in the developer portal.
type APIMProductReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Seams for tests. When nil the real Azure calls are used.
	getToken      func(ctx context.Context, clientID, tenantID string) (string, error)
	upsertProduct func(ctx context.Context, cfg apim.APIMProductConfig) error
	deleteProduct func(ctx context.Context, cfg apim.APIMProductConfig) error
}

// productFinalizer keeps an APIMProduct around until its product is removed from APIM.
const productFinalizer = "apim.operator.io/product"

// +kubebuilder:rbac:groups=apim.operator.io,resources=apimproducts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimproducts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimproducts/finalizers,verbs=update

// Reconcile creates or updates the product in APIM and, when the resource is deleted,
// removes it again unless spec.deletionPolicy is Retain.
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

	deleting := !product.DeletionTimestamp.IsZero()
	hasFinalizer := controllerutil.ContainsFinalizer(&product, productFinalizer)
	switch {
	case deleting && !hasFinalizer:
		// Nothing of ours is left in APIM to clean up.
		return ctrl.Result{}, nil
	case !deleting && !hasFinalizer:
		// Take the finalizer before the product exists in APIM so a delete can never orphan it.
		controllerutil.AddFinalizer(&product, productFinalizer)
		if err := r.Update(ctx, &product); err != nil {
			return ctrl.Result{}, err
		}
	}

	if deleting && product.Spec.DeletionPolicy == apimv1.DeletionPolicyRetain {
		logger.Info("🗑️ APIMProduct deleted with deletionPolicy Retain; the product stays in APIM",
			"name", req.NamespacedName, "productId", product.Spec.ProductID)
		return r.releaseFinalizer(ctx, &product)
	}

	operatorNamespace := getOperatorNamespace()

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: product.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "❌ Failed to get APIMService", "name", product.Spec.APIMService)
			return ctrl.Result{}, err
		}
		if deleting {
			logger.Info("⚠️ APIMService is gone, cannot remove the product from APIM; releasing the finalizer",
				"name", req.NamespacedName, "apimService", product.Spec.APIMService, "productId", product.Spec.ProductID)
			return r.releaseFinalizer(ctx, &product)
		}
		message := missingAPIMServiceMessage(product.Spec.APIMService, operatorNamespace)
		logger.Info("⏳ "+message+"; retrying", "name", req.NamespacedName)
		r.setError(ctx, &product, message)
		return ctrl.Result{RequeueAfter: requeueMissingAPIMService}, nil
	}

	logger.Info("🔗 Found APIMService", "name", apimService.Name)

	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		if deleting {
			// Without an identity this operator can never have created the product, so
			// there is nothing to remove; holding the resource would only wedge deletes.
			logger.Info("⚠️ No Azure identity configured, releasing the finalizer without touching APIM",
				"name", req.NamespacedName, "productId", product.Spec.ProductID)
			return r.releaseFinalizer(ctx, &product)
		}
		logger.Error(fmt.Errorf("missing identity env vars"), "❌ AZURE_CLIENT_ID or AZURE_TENANT_ID not set")
		r.setError(ctx, &product, errMsgMissingAzureIdentity)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	token, err := r.token(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		r.setError(ctx, &product, errMsgFailedToGetAzureToken)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

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

	if deleting {
		logger.Info("🗑️ APIMProduct is being deleted", "name", req.NamespacedName, "productId", cfg.ProductID)
		if err := r.remove(ctx, cfg); err != nil {
			logger.Error(err, "❌ Failed to delete product in APIM", "productId", cfg.ProductID)
			r.setError(ctx, &product, err.Error())
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		logger.Info("✅ Successfully deleted APIM product", "productId", cfg.ProductID)
		return r.releaseFinalizer(ctx, &product)
	}

	if err := r.upsert(ctx, cfg); err != nil {
		logger.Error(err, "❌ Failed to create product in APIM", "productId", cfg.ProductID)
		r.setError(ctx, &product, err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	logger.Info("✅ Successfully created APIM product", "productId", cfg.ProductID)
	if err := r.status(ctx, &product, phaseCreated, "Product created successfully"); err != nil {
		logger.Error(err, "❌ Failed to patch APIMProduct status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// status patches only the status subresource so spec and metadata stay untouched.
func (r *APIMProductReconciler) status(ctx context.Context, product *apimv1.APIMProduct, phase, message string) error {
	patch := client.MergeFrom(product.DeepCopy())
	product.Status.Phase = phase
	product.Status.Message = message
	return r.Status().Patch(ctx, product, patch)
}

// setError records a failure; a failing patch is logged rather than returned.
func (r *APIMProductReconciler) setError(ctx context.Context, product *apimv1.APIMProduct, message string) {
	if err := r.status(ctx, product, phaseError, message); err != nil {
		log.FromContext(ctx).Error(err, "❌ Failed to patch APIMProduct status")
	}
}

// releaseFinalizer lets the API server finish the delete.
func (r *APIMProductReconciler) releaseFinalizer(ctx context.Context, product *apimv1.APIMProduct) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(product, productFinalizer)
	if err := r.Update(ctx, product); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

func (r *APIMProductReconciler) token(ctx context.Context, clientID, tenantID string) (string, error) {
	if r.getToken != nil {
		return r.getToken(ctx, clientID, tenantID)
	}
	return identity.GetManagementToken(ctx, clientID, tenantID)
}

func (r *APIMProductReconciler) upsert(ctx context.Context, cfg apim.APIMProductConfig) error {
	if r.upsertProduct != nil {
		return r.upsertProduct(ctx, cfg)
	}
	return apim.UpsertProduct(ctx, cfg)
}

func (r *APIMProductReconciler) remove(ctx context.Context, cfg apim.APIMProductConfig) error {
	if r.deleteProduct != nil {
		return r.deleteProduct(ctx, cfg)
	}
	return apim.DeleteProduct(ctx, cfg)
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMProductReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMProduct{}).
		WithEventFilter(specOrDeletionChanged()).
		Named("apimproduct").
		Complete(r)
}
