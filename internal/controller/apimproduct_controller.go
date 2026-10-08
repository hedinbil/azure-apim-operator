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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// APIMProductReconciler reconciles APIMProduct custom resources.
// This controller manages products in Azure API Management, which are used to group
// APIs and require subscriptions for access. Products can be published or unpublished
// to control visibility in the developer portal.
type APIMProductReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Seams for tests. When nil the real Azure calls are used.
	getToken      managementTokenFunc
	upsertProduct func(ctx context.Context, cfg apim.APIMProductConfig) error
	deleteProduct func(ctx context.Context, cfg apim.APIMProductConfig) error

	// retry is how failed APIM writes back off; nil means productionRetryPolicy.
	retry *retryPolicy
	// apiReader reads the resource itself, bypassing the informer cache, so the retry
	// gate never decides on a status older than the last failure; see latestReader.
	// SetupWithManager sets it; nil reads through the embedded client.
	apiReader client.Reader
}

// productFinalizer keeps an APIMProduct around until its product is removed from APIM.
// Only a resource with spec.deletionPolicy Delete carries it: a Retain resource must be
// deletable even when this operator is absent, broken or without credentials.
const productFinalizer = "apim.operator.io/product"

// +kubebuilder:rbac:groups=apim.operator.io,resources=apimproducts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimproducts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimproducts/finalizers,verbs=update

// Reconcile creates or updates the product in APIM and, when the resource is deleted,
// removes it again if spec.deletionPolicy is Delete. Retain (the default) never touches
// APIM on the way out.
func (r *APIMProductReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var product apimv1.APIMProduct
	if err := latestReader(r.apiReader, r.Client).Get(ctx, req.NamespacedName, &product); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("🧹 APIMProduct deleted, skipping", "name", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMProduct")
		return ctrl.Result{}, err
	}

	deleting := !product.DeletionTimestamp.IsZero()
	hasFinalizer := controllerutil.ContainsFinalizer(&product, productFinalizer)
	wantsDelete := product.Spec.DeletionPolicy == apimv1.DeletionPolicyDelete
	switch {
	case deleting && !hasFinalizer:
		// Nothing of ours is left to clean up; the API server finishes the delete on its own.
		return ctrl.Result{}, nil
	case deleting && !wantsDelete:
		// Retain - or switched to Retain while a Delete was refused - lets the resource go
		// and leaves the product where it is.
		logger.Info("🗑️ APIMProduct deleted with deletionPolicy Retain; the product stays in APIM",
			"name", req.NamespacedName, "productId", product.Spec.ProductID)
		return r.releaseFinalizer(ctx, &product)
	case !deleting && wantsDelete && !hasFinalizer:
		// Take the finalizer before the product exists in APIM so a delete can never orphan it.
		controllerutil.AddFinalizer(&product, productFinalizer)
		if err := r.Update(ctx, &product); err != nil {
			return ctrl.Result{}, err
		}
	case !deleting && !wantsDelete && hasFinalizer:
		// Switched from Delete to Retain: drop the finalizer now, so deleting the resource
		// later never waits on this operator.
		controllerutil.RemoveFinalizer(&product, productFinalizer)
		if err := r.Update(ctx, &product); err != nil {
			return ctrl.Result{}, err
		}
	}

	operatorNamespace := getOperatorNamespace()

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: product.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "❌ Failed to get APIMService", "name", product.Spec.APIMService)
			return ctrl.Result{}, err
		}
		if deleting {
			logger.Error(err, "⚠️ APIMService is gone, cannot remove the product from APIM; releasing the finalizer and leaving the product in place",
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
			// Without an identity this operator cannot remove the product. Holding the
			// resource would wedge the delete (of the namespace, too) until someone fixes
			// the operator's configuration, so it lets go and leaves the product in APIM.
			logger.Error(fmt.Errorf("no Azure identity configured"),
				"⚠️ Cannot remove the product from APIM; releasing the finalizer and leaving the product in place",
				"name", req.NamespacedName, "productId", product.Spec.ProductID)
			return r.releaseFinalizer(ctx, &product)
		}
		logger.Error(fmt.Errorf("missing identity env vars"), "❌ AZURE_CLIENT_ID or AZURE_TENANT_ID not set")
		r.setError(ctx, &product, errMsgMissingAzureIdentity)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Gate before the token: a product that is backing off, Stalled or Invalid does not
	// even fetch one. The upsert and the finalizer's delete share one retry state; the
	// generation bump that comes with a delete resets it, so a product that stalled while
	// being created still gets a fresh set of attempts at being removed.
	operation := "upsert"
	if deleting {
		operation = "delete"
	}
	w := r.retry.begin(logger, "APIMProduct", &product, "productID", product.Spec.ProductID, "operation", operation)
	if proceed, result := w.gate(product.Status.RetryStatus, product.Generation != product.Status.ObservedGeneration); !proceed {
		// Put the phase back if an error path above overwrote it while the product was held.
		if phase, message, ok := w.heldStatus(product.Status.Phase); ok {
			if err := r.status(ctx, &product, phase, message); err != nil {
				logger.Error(err, "❌ Failed to patch APIMProduct status", "productID", product.Spec.ProductID)
			}
		}
		return result, nil
	}

	token, err := r.getToken.get(ctx, clientID, tenantID)
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
		return r.removeFromAPIM(ctx, &product, w, cfg)
	}

	w.starting()
	if err := r.upsert(ctx, cfg); err != nil {
		return r.writeFailed(ctx, &product, w, err, "Failed to create product in APIM")
	}
	patch := client.MergeFrom(product.DeepCopy())
	w.succeeded(&product.Status.RetryStatus)
	product.Status.ObservedGeneration = product.Generation
	product.Status.Phase = phaseCreated
	product.Status.Message = "Product created successfully"
	outCtx, cancel := outcomeContext(ctx)
	defer cancel()
	if err := r.Status().Patch(outCtx, &product, patch); err != nil {
		logger.Error(err, "❌ Failed to patch APIMProduct status")
		// APIM has the write; only recording it failed. Never hand the error back: that
		// would put controller-runtime's rate limiter, with no attempt limit, in charge of
		// writing to APIM again. Check again after one backoff step instead.
		return ctrl.Result{RequeueAfter: w.policy.BaseDelay}, nil
	}
	return ctrl.Result{}, nil
}

// removeFromAPIM deletes the product from APIM and then lets the resource go.
func (r *APIMProductReconciler) removeFromAPIM(ctx context.Context, product *apimv1.APIMProduct, w *apimWrite, cfg apim.APIMProductConfig) (ctrl.Result, error) {
	w.starting()
	if err := r.remove(ctx, cfg); err != nil {
		return r.writeFailed(ctx, product, w, err, "Failed to delete product in APIM")
	}
	// The resource is about to go, so the cleared retry state is only logged, not patched.
	w.succeeded(&product.Status.RetryStatus)
	// Released even when the reconcile has timed out or the operator is stopping. A failure
	// is retried after one backoff step, never through the rate limiter; the DELETE that
	// comes with the retry is harmless, as a 404 counts as removed.
	outCtx, cancel := outcomeContext(ctx)
	defer cancel()
	result, err := r.releaseFinalizer(outCtx, product)
	if err != nil {
		log.FromContext(ctx).Error(err, "❌ Product removed from APIM but releasing the finalizer failed", "productID", cfg.ProductID)
		return ctrl.Result{RequeueAfter: w.policy.BaseDelay}, nil
	}
	return result, nil
}

// writeFailed records a failed APIM write: Backoff until the next attempt time, Stalled
// after too many transient failures in a row, or Invalid when APIM rejected the request.
// It returns the requeue the retry policy decided on and never the error itself, which
// would put controller-runtime's own rate limiter back in charge.
func (r *APIMProductReconciler) writeFailed(ctx context.Context, product *apimv1.APIMProduct, w *apimWrite, err error, step string) (ctrl.Result, error) {
	patch := client.MergeFrom(product.DeepCopy())
	out := w.failed(&product.Status.RetryStatus, err)
	product.Status.ObservedGeneration = product.Generation
	product.Status.Phase = out.Phase
	// APIMProductStatus has no lastError, so the error itself goes into the message.
	product.Status.Message = out.statusMessage(step, err)
	if !product.DeletionTimestamp.IsZero() && out.Result.IsZero() {
		// Stalled or Invalid while deleting holds the finalizer until someone acts; say how
		// to let the resource go without touching APIM.
		product.Status.Message += fmt.Sprintf("; or set spec.deletionPolicy to %s to delete the resource and keep the product",
			apimv1.DeletionPolicyRetain)
	}
	// Record the failure even when the reconcile has timed out or the operator is stopping.
	outCtx, cancel := outcomeContext(ctx)
	defer cancel()
	if patchErr := r.Status().Patch(outCtx, product, patch); patchErr != nil {
		log.FromContext(ctx).Error(patchErr, "❌ Failed to patch APIMProduct status")
	}
	return out.Result, nil
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
	if r.apiReader == nil {
		r.apiReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMProduct{}).
		WithEventFilter(predicate.And(logRetainedOnDelete(), specOrDeletionChanged())).
		Named("apimproduct").
		WithOptions(apimWriterOptions()).
		Complete(r)
}

// logRetainedOnDelete records what a deleted APIMProduct leaves behind in APIM. A Retain
// resource carries no finalizer, so this cache event is the last moment the operator still
// knows which product it was; the reconcile that follows only sees NotFound. The line is
// the audit trail for cleaning APIM up by hand later.
func logRetainedOnDelete() predicate.Predicate {
	return predicate.Funcs{
		DeleteFunc: func(e event.DeleteEvent) bool {
			if product, ok := e.Object.(*apimv1.APIMProduct); ok &&
				product.Spec.DeletionPolicy != apimv1.DeletionPolicyDelete {
				ctrl.Log.WithName("apimproduct").Info("📦 APIMProduct removed; its product was retained in APIM",
					"name", product.Name, "namespace", product.Namespace,
					"productId", product.Spec.ProductID, "apimService", product.Spec.APIMService)
			}
			return true
		},
	}
}
