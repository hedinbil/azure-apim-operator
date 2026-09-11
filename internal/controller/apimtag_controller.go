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
	"sigs.k8s.io/controller-runtime/pkg/log"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
	"github.com/hedinit/azure-apim-operator/internal/identity"
)

// APIMTagReconciler reconciles APIMTag custom resources.
// This controller manages tags in Azure API Management, which are used to categorize
// and organize APIs for easier management and discovery. Tags help group related
// APIs together without requiring subscriptions like products do.
type APIMTagReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.operator.io,resources=apimtags,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimtags/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimtags/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the APIMTag object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *APIMTagReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tag apimv1.APIMTag
	if err := r.Get(ctx, req.NamespacedName, &tag); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("🧹 APIMTag deleted, skipping", "name", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMTag")
		return ctrl.Result{}, err
	}

	operatorNamespace := getOperatorNamespace()

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: tag.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "❌ Failed to get APIMService", "name", tag.Spec.APIMService)
			return ctrl.Result{}, err
		}
		message := missingAPIMServiceMessage(tag.Spec.APIMService, operatorNamespace)
		logger.Info("⏳ "+message+"; retrying", "name", req.NamespacedName)
		statusPatch := client.MergeFrom(tag.DeepCopy())
		tag.Status.Phase = phaseError
		tag.Status.Message = message
		if patchErr := r.Status().Patch(ctx, &tag, statusPatch); patchErr != nil {
			logger.Error(patchErr, "❌ Failed to patch APIMTag status")
		}
		return ctrl.Result{RequeueAfter: requeueMissingAPIMService}, nil
	}

	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		logger.Error(fmt.Errorf("missing identity env vars"), "❌ AZURE_CLIENT_ID or AZURE_TENANT_ID not set")
		// Use Patch to update only status without touching spec fields.
		statusPatch := client.MergeFrom(tag.DeepCopy())
		tag.Status.Phase = phaseError
		tag.Status.Message = errMsgMissingAzureIdentity
		_ = r.Status().Patch(ctx, &tag, statusPatch)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		// Use Patch to update only status without touching spec fields.
		statusPatch := client.MergeFrom(tag.DeepCopy())
		tag.Status.Phase = phaseError
		tag.Status.Message = errMsgFailedToGetAzureToken
		_ = r.Status().Patch(ctx, &tag, statusPatch)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	cfg := apim.APIMTagConfig{
		SubscriptionID: apimService.Spec.Subscription,
		ResourceGroup:  apimService.Spec.ResourceGroup,
		ServiceName:    tag.Spec.APIMService,
		TagID:          tag.Spec.TagID,
		DisplayName:    tag.Spec.DisplayName,
		BearerToken:    token,
	}

	if err := apim.UpsertTag(ctx, cfg); err != nil {
		logger.Error(err, "❌ Failed to upsert APIM tag", "tagID", cfg.TagID)
		tag.Status.Phase = phaseError
		tag.Status.Message = err.Error()
	} else {
		logger.Info("✅ Successfully upserted APIM tag", "tagID", cfg.TagID)
		tag.Status.Phase = phaseCreated
		tag.Status.Message = "Tag created or updated"
	}

	// Use Patch to update only status without touching spec fields.
	statusPatch := client.MergeFrom(tag.DeepCopy())
	if err := r.Status().Patch(ctx, &tag, statusPatch); err != nil {
		logger.Error(err, "❌ Failed to patch APIMTag status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMTagReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMTag{}).
		WithEventFilter(specOrDeletionChanged()).
		Named("apimtag").
		Complete(r)
}
