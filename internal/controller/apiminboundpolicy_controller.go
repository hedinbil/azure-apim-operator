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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// APIMInboundPolicyReconciler reconciles a APIMInboundPolicy object
type APIMInboundPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// getToken obtains the Azure Management token; nil means identity.GetManagementToken.
	// Tests set it so the write path can run against a fake ARM.
	getToken managementTokenFunc
	// retry is how failed policy writes back off; nil means productionRetryPolicy.
	retry *retryPolicy
	// apiReader reads the resource itself, bypassing the informer cache, so the retry
	// gate never decides on a status older than the last failure; see latestReader.
	// SetupWithManager sets it; nil reads through the embedded client.
	apiReader client.Reader
}

// +kubebuilder:rbac:groups=apim.operator.io,resources=apiminboundpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apiminboundpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apiminboundpolicies/finalizers,verbs=update

// Reconcile sets the inbound policy on the API (or one of its operations) in APIM when the
// retry policy allows a write (see retry.go).
func (r *APIMInboundPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var policy apimv1.APIMInboundPolicy
	if err := latestReader(r.apiReader, r.Client).Get(ctx, req.NamespacedName, &policy); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("🧹 APIMInboundPolicy deleted, skipping", "name", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMInboundPolicy")
		return ctrl.Result{}, err
	}

	operatorNamespace := getOperatorNamespace()

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: policy.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "❌ Failed to get APIMService", "name", policy.Spec.APIMService, "apiID", policy.Spec.APIID)
			return ctrl.Result{}, err
		}
		message := missingAPIMServiceMessage(policy.Spec.APIMService, operatorNamespace)
		logger.Info("⏳ "+message+"; retrying", "name", req.NamespacedName, "apiID", policy.Spec.APIID)
		statusPatch := client.MergeFrom(policy.DeepCopy())
		policy.Status.Phase = phaseError
		policy.Status.Message = message
		if patchErr := r.Status().Patch(ctx, &policy, statusPatch); patchErr != nil {
			logger.Error(patchErr, "❌ Failed to patch APIMInboundPolicy status")
		}
		return ctrl.Result{RequeueAfter: requeueMissingAPIMService}, nil
	}

	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		logger.Error(fmt.Errorf("missing identity env vars"), "❌ AZURE_CLIENT_ID or AZURE_TENANT_ID not set", "apiID", policy.Spec.APIID)
		// Use Patch to update only status without touching spec fields.
		statusPatch := client.MergeFrom(policy.DeepCopy())
		policy.Status.Phase = phaseError
		policy.Status.Message = errMsgMissingAzureIdentity
		_ = r.Status().Patch(ctx, &policy, statusPatch)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Decide before anything else touches Azure: a policy that is backing off, Stalled or
	// Invalid does not even fetch a token. A spec change or a new retry annotation value
	// starts it over.
	writeKV := []any{"apiID", policy.Spec.APIID}
	if policy.Spec.OperationID != "" {
		writeKV = append(writeKV, "operationID", policy.Spec.OperationID)
	}
	write := r.retry.begin(logger, "APIMInboundPolicy", &policy, writeKV...)
	if proceed, result := write.gate(policy.Status.RetryStatus, policy.Generation != policy.Status.ObservedGeneration); !proceed {
		// Put the phase back if an error path above overwrote it while the policy was held.
		if phase, message, ok := write.heldStatus(policy.Status.Phase); ok {
			statusPatch := client.MergeFrom(policy.DeepCopy())
			policy.Status.Phase = phase
			policy.Status.Message = message
			if err := r.Status().Patch(ctx, &policy, statusPatch); err != nil {
				logger.Error(err, "❌ Failed to patch APIMInboundPolicy status", "apiID", policy.Spec.APIID)
			}
		}
		return result, nil
	}

	token, err := r.getToken.get(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token", "apiID", policy.Spec.APIID)
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

	write.starting()
	upsertErr := apim.UpsertInboundPolicy(ctx, cfg)

	// Record the outcome even when the reconcile has timed out or the operator is stopping.
	outCtx, cancel := outcomeContext(ctx)
	defer cancel()

	// Take the patch base before touching the status, so the patch carries the changes.
	statusPatch := client.MergeFrom(policy.DeepCopy())
	policy.Status.ObservedGeneration = policy.Generation

	if upsertErr != nil {
		// Back off, stall or give up as the shared retry handling decides. The error is never
		// returned: that would hand the requeue back to controller-runtime's rate limiter.
		outcome := write.failed(&policy.Status.RetryStatus, upsertErr)
		policy.Status.Phase = outcome.Phase
		// APIMInboundPolicyStatus has no lastError, so the error itself goes into the message.
		policy.Status.Message = outcome.statusMessage("Failed to create or update inbound policy in APIM", upsertErr)
		if err := r.Status().Patch(outCtx, &policy, statusPatch); err != nil {
			// The failure count is lost without the patch, but the requeue still waits out
			// this attempt's backoff instead of retrying at once.
			logger.Error(err, "❌ Failed to patch APIMInboundPolicy status", "apiID", cfg.APIID)
		}
		return outcome.Result, nil
	}

	write.succeeded(&policy.Status.RetryStatus)
	if cfg.OperationID != "" {
		policy.Status.Message = fmt.Sprintf("APIM Inbound Policy created or updated for operation %s", cfg.OperationID)
	} else {
		policy.Status.Message = "APIM Inbound Policy created or updated"
	}
	policy.Status.Phase = phaseCreated

	// Use Patch to update only status without touching spec fields.
	if err := r.Status().Patch(outCtx, &policy, statusPatch); err != nil {
		logger.Error(err, "❌ Failed to patch APIMInboundPolicy status", "apiID", cfg.APIID)
		// APIM has the write; only recording it failed. Never hand the error back: that
		// would put controller-runtime's rate limiter, with no attempt limit, in charge of
		// writing to APIM again. Check again after one backoff step instead.
		return ctrl.Result{RequeueAfter: write.policy.BaseDelay}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMInboundPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.apiReader == nil {
		r.apiReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMInboundPolicy{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return true },
			UpdateFunc:  apimInboundPolicyUpdateFilter(),
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		WithOptions(apimWriterOptions()).
		Named("apiminboundpolicy").
		Complete(r)
}

// apimInboundPolicyUpdateFilter decides which policy updates reach Reconcile.
func apimInboundPolicyUpdateFilter() func(e event.UpdateEvent) bool {
	return func(e event.UpdateEvent) bool {
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
		// Reconcile if any spec field changed, or if the retry annotation asks to start
		// a Stalled or Invalid policy over (a metadata-only change). The generation
		// covers spec fields not listed here (e.g. deletionPolicy): without it
		// status.observedGeneration falls behind, and the next reconcile for any other
		// reason would read that as a spec change and reset a Stalled policy's failures.
		return oldPolicy.Generation != newPolicy.Generation ||
			oldPolicy.Spec.APIMService != newPolicy.Spec.APIMService ||
			oldPolicy.Spec.APIID != newPolicy.Spec.APIID ||
			oldPolicy.Spec.OperationID != newPolicy.Spec.OperationID ||
			oldPolicy.Spec.PolicyContent != newPolicy.Spec.PolicyContent ||
			retryAnnotationChanged(e)
	}
}
