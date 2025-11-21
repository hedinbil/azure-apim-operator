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

// APIMInboundPolicyReconciler reconciles a APIMInboundPolicy object
type APIMInboundPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.operator.io,resources=apiminboundpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apiminboundpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apiminboundpolicies/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the APIMInboundPolicy object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *APIMInboundPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var policy apimv1.APIMInboundPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("🧹 APIMInboundPolicy deleted, skipping", "name", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMInboundPolicy")
		return ctrl.Result{}, err
	}

	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		logger.Error(err, "❌ Failed to read operator namespace")
		return ctrl.Result{}, fmt.Errorf("read operator namespace: %w", err)
	}
	operatorNamespace := strings.TrimSpace(string(nsBytes))

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: policy.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		logger.Error(err, "❌ Failed to get APIMService", "name", policy.Spec.APIMService)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		return ctrl.Result{}, fmt.Errorf("missing AZURE_CLIENT_ID or AZURE_TENANT_ID")
	}

	token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		// Use Patch to update only status without touching spec fields.
		statusPatch := client.MergeFrom(policy.DeepCopy())
		policy.Status.Phase = phaseError
		policy.Status.Message = errMsgFailedToGetAzureToken
		_ = r.Status().Patch(ctx, &policy, statusPatch)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	cfg := apim.APIMInboundPolicyConfig{
		SubscriptionID: apimService.Spec.Subscription,
		ResourceGroup:  apimService.Spec.ResourceGroup,
		ServiceName:    policy.Spec.APIMService,
		APIID:          policy.Spec.APIID,
		OperationID:    policy.Spec.OperationID,
		PolicyContent:  policy.Spec.PolicyContent,
		BearerToken:    token,
	}

	if err := apim.UpsertInboundPolicy(ctx, cfg); err != nil {
		if cfg.OperationID != "" {
			logger.Error(err, "❌ Failed to upsert APIM Inbound Policy", "apiID", cfg.APIID, "operationID", cfg.OperationID)
		} else {
			logger.Error(err, "❌ Failed to upsert APIM Inbound Policy", "apiID", cfg.APIID)
		}
		policy.Status.Phase = phaseError
		policy.Status.Message = err.Error()
	} else {
		if cfg.OperationID != "" {
			logger.Info("✅ Successfully upserted APIM Inbound Policy", "apiID", cfg.APIID, "operationID", cfg.OperationID)
			policy.Status.Message = fmt.Sprintf("APIM Inbound Policy created or updated for operation %s", cfg.OperationID)
		} else {
			logger.Info("✅ Successfully upserted APIM Inbound Policy", "apiID", cfg.APIID)
			policy.Status.Message = "APIM Inbound Policy created or updated"
		}
		policy.Status.Phase = phaseCreated
	}

	// Use Patch to update only status without touching spec fields.
	statusPatch := client.MergeFrom(policy.DeepCopy())
	if err := r.Status().Patch(ctx, &policy, statusPatch); err != nil {
		logger.Error(err, "❌ Failed to patch APIMInboundPolicy status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMInboundPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMInboundPolicy{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return true },
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Only reconcile on policy updates when the spec changes
				// This ensures policy changes are picked up and applied to APIM
				oldPolicy, ok := e.ObjectOld.(*apimv1.APIMInboundPolicy)
				if !ok {
					return false
				}
				newPolicy, ok := e.ObjectNew.(*apimv1.APIMInboundPolicy)
				if !ok {
					return false
				}
				// Reconcile if any spec field changed
				return oldPolicy.Spec.APIMService != newPolicy.Spec.APIMService ||
					oldPolicy.Spec.APIID != newPolicy.Spec.APIID ||
					oldPolicy.Spec.OperationID != newPolicy.Spec.OperationID ||
					oldPolicy.Spec.PolicyContent != newPolicy.Spec.PolicyContent
			},
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		Named("apiminboundpolicy").
		Complete(r)
}
