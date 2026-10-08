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
	"errors"
	"fmt"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// APIMAPIDeploymentReconciler reconciles APIMAPIDeployment custom resources.
// This controller handles the complete workflow of deploying an API to Azure API Management:
//  1. Fetching the OpenAPI definition
//  2. Importing it into APIM, with its path, backend service URL and subscription
//     requirement in the same write, and following the import while APIM runs it
//  3. Associating products and tags
//  4. Updating the APIMAPI status with host information
//  5. Persisting deployment status so reconciliation progress is inspectable
type APIMAPIDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// fetcher downloads tenant OpenAPI documents; nil selects the guarded default.
	fetcher *openAPIFetcher
	// getToken obtains the Azure management token; nil selects identity.GetManagementToken.
	getToken managementTokenFunc
	// retry is how failed APIM writes are retried; nil selects productionRetryPolicy.
	retry *retryPolicy
	// apiReader reads the resource itself, bypassing the informer cache, so the retry
	// gate never decides on a status older than the last failure; see latestReader.
	// SetupWithManager sets it; nil reads through the embedded client.
	apiReader client.Reader
}

func (r *APIMAPIDeploymentReconciler) openAPI() *openAPIFetcher {
	if r.fetcher != nil {
		return r.fetcher
	}
	return defaultOpenAPIFetcher
}

// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments/finalizers,verbs=update
// APIMService has no controller of its own; it is a configuration record that
// this controller and the product, tag and policy controllers read to locate
// the APIM instance. Read-only: nothing writes it (APIM-17).
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimservices,verbs=get;list;watch

// Reconcile brings the API in APIM in line with the deployment: it writes only when the
// desired state (spec, APIM location and OpenAPI document) differs from the one last
// applied, and only when the retry policy allows a write (see retry.go).
//
//nolint:gocyclo // one long state machine; splitting it by phase is tracked in the operator review roadmap
func (r *APIMAPIDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("apimapideployment_controller")

	// Fetch the APIMAPIDeployment resource that triggered this reconciliation.
	var deployment apimv1.APIMAPIDeployment
	if err := latestReader(r.apiReader, r.Client).Get(ctx, req.NamespacedName, &deployment); err != nil {
		logger.Info("ℹ️ Unable to fetch APIMAPIDeployment")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("🧩 Loaded APIMAPIDeployment",
		"name", deployment.Name,
		"namespace", deployment.Namespace,
		"apimApiName", deployment.Spec.APIMAPIName,
		"apiID", deployment.Spec.APIID,
		"revision", deployment.Spec.Revision,
		"routePrefix", deployment.Spec.RoutePrefix,
		"type", deployment.Spec.Type,
		"openApiUrl", apim.RedactURL(deployment.Spec.OpenAPIDefinitionURL),
		"subscriptionRequired", deployment.Spec.SubscriptionRequired,
	)

	// Fetch the associated APIMAPI resource to update its status after deployment.
	apimAPIName := deployment.Spec.APIMAPIName
	if apimAPIName == "" {
		apimAPIName = deployment.Name
	}

	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: apimAPIName, Namespace: req.Namespace}, &apimApi); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("ℹ️ APIMAPI not found, skipping revision creation", "apiID", deployment.Spec.APIID, "apimApiName", apimAPIName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMAPI", "apiID", deployment.Spec.APIID, "apimApiName", apimAPIName)
		return ctrl.Result{}, err
	}
	logger.Info("🔗 Found APIMAPI for deployment", "apimapi", apimApi.Name, "status", apimApi.Status.Status, "apiID", deployment.Spec.APIID, "apimApiName", apimAPIName)

	attemptTime := time.Now().UTC().Format(time.RFC3339)
	matchedReplicaSets, err := findMatchingReplicaSetsForAPIMAPI(ctx, r.Client, &apimApi)
	if err != nil {
		logger.Error(err, "❌ Failed to match ReplicaSets for APIMAPI", "apiID", deployment.Spec.APIID, "apimApiName", apimAPIName)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = phaseError
			status.Status = phaseError
			status.Message = "Failed to resolve matching ReplicaSets"
			status.LastError = err.Error()
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = nil
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	matchedReplicaSetNames := matchedReplicaSetNames(matchedReplicaSets)
	if len(matchedReplicaSets) == 0 {
		message := fmt.Sprintf("Selector matched 0 ReplicaSets in namespace %s", deployment.Namespace)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = apimDeploymentPhaseWaitingForMatch
			status.Status = apimDeploymentStatusPending
			status.Message = message
			status.LastError = ""
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = nil
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		logger.Info("⏳ Waiting for selector match", "apiID", deployment.Spec.APIID, "apimApiName", apimAPIName)
		return ctrl.Result{RequeueAfter: requeueWaitingForWorkload}, nil
	}

	readyPod, err := findReadyPodForReplicaSets(ctx, r.Client, matchedReplicaSets)
	if err != nil {
		logger.Error(err, "❌ Failed to inspect matched ReplicaSet pods", "apiID", deployment.Spec.APIID, "apimApiName", apimAPIName)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = phaseError
			status.Status = phaseError
			status.Message = "Failed to inspect matched ReplicaSet pods"
			status.LastError = err.Error()
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if readyPod == nil {
		message := fmt.Sprintf("Matched ReplicaSets %v but no ready pods were found yet", matchedReplicaSetNames)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = apimDeploymentPhaseWaitingForReadyPod
			status.Status = apimDeploymentStatusPending
			status.Message = message
			status.LastError = ""
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		logger.Info("⏳ Waiting for ready pod", "apiID", deployment.Spec.APIID, "matchedReplicaSets", matchedReplicaSetNames)
		// The ReplicaSet signal and the pod's Ready condition reach the cache separately; a
		// ReplicaSet with one replica signals only once, so look again rather than wait for
		// the next rollout.
		return ctrl.Result{RequeueAfter: requeueWaitingForWorkload}, nil
	}

	// During a rolling update the OpenAPI URL, a Service, still reaches pods of the old
	// version, and the document fetched from one of them would be imported as if it were
	// new. Wait until the old version's pods are gone.
	if old := replicaSetsStillRollingOut(matchedReplicaSets); len(old) > 0 {
		message := fmt.Sprintf("Waiting for the rollout to finish: older ReplicaSets %v still have ready pods", old)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = apimDeploymentPhaseWaitingForRollout
			status.Status = apimDeploymentStatusPending
			status.Message = message
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
		}); statusErr != nil {
			logger.Error(statusErr, "❌ Failed to patch APIMAPIDeployment status", "apiID", deployment.Spec.APIID)
		}
		logger.Info("⏳ "+message, "apiID", deployment.Spec.APIID)
		return ctrl.Result{RequeueAfter: requeueWaitingForRollout}, nil
	}

	operatorNamespace := getOperatorNamespace()

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: deployment.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		message := fmt.Sprintf("Referenced APIMService %q was not found in namespace %s", deployment.Spec.APIMService, operatorNamespace)
		if !apierrors.IsNotFound(err) {
			message = "Failed to fetch referenced APIMService"
		}
		logger.Error(err, "❌ Failed to get APIMService", "apiID", deployment.Spec.APIID, "apimService", deployment.Spec.APIMService)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = phaseError
			status.Status = phaseError
			status.Message = message
			status.LastError = err.Error()
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	if deployment.Spec.Subscription != apimService.Spec.Subscription || deployment.Spec.ResourceGroup != apimService.Spec.ResourceGroup {
		specPatch := client.MergeFrom(deployment.DeepCopy())
		deployment.Spec.Subscription = apimService.Spec.Subscription
		deployment.Spec.ResourceGroup = apimService.Spec.ResourceGroup
		if err := r.Patch(ctx, &deployment, specPatch); err != nil {
			logger.Error(err, "❌ Failed to sync APIM service location onto deployment", "apiID", deployment.Spec.APIID)
			return ctrl.Result{}, err
		}
	}

	// Step 1: Fetch the OpenAPI definition from the URL the tenant declared. One bounded
	// attempt per reconcile; a failure comes back through the workqueue after a pause.
	// A websocket API has no OpenAPI document: APIM creates it from the spec alone, so
	// the fetch is skipped and the hash input for it stays empty.
	isWebSocket := deployment.Spec.Type == apimv1.APITypeWebSocket
	var openApiContent []byte
	openAPIHash := ""
	if isWebSocket {
		logger.Info("🔌 WebSocket API; skipping OpenAPI fetch", "apiID", deployment.Spec.APIID)
	} else {
		openApiURL := deployment.Spec.OpenAPIDefinitionURL
		logger.Info("📡 Fetching OpenAPI definition", "url", apim.RedactURL(openApiURL), "apiID", deployment.Spec.APIID)
		openApiContent, err = r.openAPI().Fetch(ctx, openApiURL)
		if err != nil {
			logger.Error(err, "❌ Failed to fetch OpenAPI definition", "apiID", deployment.Spec.APIID)
			if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
				status.Phase = phaseError
				status.Status = phaseError
				status.Message = "Failed to fetch OpenAPI definition"
				status.LastError = err.Error()
				status.LastAttemptAt = attemptTime
				status.ObservedGeneration = apimApi.Generation
				status.MatchedReplicaSets = matchedReplicaSetNames
			}); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: requeueFetchFailure}, nil
		}
		openAPIHash = sha256Hex(openApiContent)
		logger.Info("📥 OpenAPI definition downloaded",
			"bytes", len(openApiContent),
			"url", apim.RedactURL(openApiURL),
			"apiID", deployment.Spec.APIID,
		)
	}
	desiredHash, err := buildDesiredAPIMStateHash(&deployment.Spec, apimService.Spec.Subscription, apimService.Spec.ResourceGroup, openAPIHash)
	if err != nil {
		logger.Error(err, "❌ Failed to build desired APIM state hash", "apiID", deployment.Spec.APIID)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = phaseError
			status.Status = phaseError
			status.Message = "Failed to hash desired APIM state"
			status.LastError = err.Error()
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
			status.OpenAPIHash = openAPIHash
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	// An import APIM is still running may change the API after this point, so the
	// applied hash only proves the API is in sync when no import is pending. An APIMAPI
	// without its hosts (its status patch failed when the write succeeded) is not in sync
	// either: the steps after the import run again to fill them in, without importing.
	pending := deployment.Status.PendingImport
	if pending == nil && deployment.Status.AppliedHash == desiredHash && apimApi.Status.ApiHost != "" {
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = apimDeploymentPhaseSucceeded
			status.Status = "OK"
			status.Message = "No changes detected; APIM is already in sync"
			status.LastError = ""
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
			status.OpenAPIHash = openAPIHash
			status.DesiredHash = desiredHash
			// APIM already holds this state, so failures recorded for another one no
			// longer apply (e.g. a spec that failed and was then reverted).
			status.ConsecutiveFailures = 0
			status.NextAttemptAt = ""
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		r.setAPIMAPIStatus(ctx, logger, &apimApi, apimAPIStatusOK)
		logger.Info("✅ APIM already in sync; skipping import", "apiID", deployment.Spec.APIID, "desiredHash", desiredHash)
		return ctrl.Result{}, nil
	}

	// Decide whether APIM may be written to at all before anything is persisted: a
	// deployment still backing off, Stalled or Invalid returns here without a token, an
	// APIM call or a status write. A new desired hash or a new apim.operator.io/retry
	// value clears the failures, except a new OpenAPI document alone during a backoff (see
	// deploymentSpecChanged); the hash is compared with status.desiredHash before the
	// Importing patch below overwrites it.
	w := r.retry.begin(logger, "APIMAPIDeployment", &deployment, "apiID", deployment.Spec.APIID)
	specChanged := deploymentSpecChanged(&deployment.Spec, &deployment.Status,
		apimService.Spec.Subscription, apimService.Spec.ResourceGroup, desiredHash)
	if proceed, result := w.gate(deployment.Status.RetryStatus, specChanged); !proceed {
		// Put the phase back if a path above (missing APIMService, failed OpenAPI fetch,
		// no ready pod) overwrote it while the deployment was held. lastError keeps the
		// last error seen.
		if phase, message, ok := w.heldStatus(deployment.Status.Phase); ok {
			if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
				status.Phase = phase
				status.Status = phaseError
				status.Message = message
				status.MatchedReplicaSets = matchedReplicaSetNames
			}); statusErr != nil {
				logger.Error(statusErr, "❌ Failed to patch APIMAPIDeployment status", "apiID", deployment.Spec.APIID)
			}
		}
		if p := deployment.Status.Phase; p == phaseStalled || p == phaseInvalid {
			// Repairs an APIMAPI whose Error status failed to stick when the write stopped.
			r.setAPIMAPIStatus(ctx, logger, &apimApi, apimAPIStatusError)
		}
		return result, nil
	}

	// A deployment waiting for an import APIM is still running writes its status once
	// it knows how that import stands, below; the same reset goes with it.
	if pending == nil {
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = apimDeploymentPhaseImporting
			status.Status = apimDeploymentStatusPending
			status.Message = "Reconciling desired API state in APIM"
			status.LastError = ""
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
			status.OpenAPIHash = openAPIHash
			status.DesiredHash = desiredHash
			// From here APIM may hold a mix of the old and the new state: only a completed
			// write proves anything is applied. Without this, a change that failed halfway
			// and was then reverted would read as "already in sync" while APIM still holds
			// the change.
			status.AppliedHash = ""
			// The API itself is only known to be written for this desired state when the
			// import of it finished (status.importedHash); one for any other state is about
			// to be overwritten, or was overwritten partway by a write that failed.
			if status.ImportedHash != desiredHash {
				status.ImportedHash = ""
			}
			// Persist the reset together with the new desired hash, so a crash mid-import
			// cannot leave failures of the old state counting against the new one.
			w.prepare(&status.RetryStatus)
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}

	// failWrite records a failed APIM call through the shared retry handling: Backoff
	// until status.nextAttemptAt, Stalled after too many transient failures in a row, or
	// Invalid when APIM rejected the request. step says which call failed. It returns
	// what Reconcile returns; the error itself is never handed back, which would put
	// controller-runtime's rate limiter back in charge of the retries. A failing status
	// patch is logged for the same reason: the failure count is lost for this attempt,
	// but the requeue still waits out its backoff instead of re-importing at once. also
	// lets a caller change more of the status in the same patch. failWriteNotBefore keeps the
	// next attempt at least floor away, for a write that may still be running in APIM.
	failWriteNotBefore := func(floor time.Duration, step string, err error, also ...func(*apimv1.APIMAPIDeploymentStatus)) (ctrl.Result, error) {
		outCtx, cancel := outcomeContext(ctx)
		defer cancel()
		var out writeOutcome
		if statusErr := updateAPIMAPIDeploymentStatus(outCtx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			out = w.failedNotBefore(&status.RetryStatus, err, floor)
			status.Phase = out.Phase
			status.Status = phaseError
			status.Message = step + ": " + out.Message
			status.LastError = err.Error()
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
			status.OpenAPIHash = openAPIHash
			status.DesiredHash = desiredHash
			for _, mutate := range also {
				mutate(status)
			}
		}); statusErr != nil {
			logger.Error(statusErr, "❌ Failed to patch APIMAPIDeployment status", "apiID", deployment.Spec.APIID)
		}
		if out.Phase == phaseStalled || out.Phase == phaseInvalid {
			// The APIMAPI is what the team's ArgoCD app manages; its health reads
			// status.status. A deployment that has stopped writing must show there too.
			r.setAPIMAPIStatus(outCtx, logger, &apimApi, apimAPIStatusError)
		}
		return out.Result, nil
	}
	failWrite := func(step string, err error, also ...func(*apimv1.APIMAPIDeploymentStatus)) (ctrl.Result, error) {
		return failWriteNotBefore(0, step, err, also...)
	}

	// Step 2: Acquire an Azure management token for authenticating with the APIM Management API.
	// The token is obtained using workload identity credentials.
	clientID := os.Getenv("AZURE_CLIENT_ID")
	tenantID := os.Getenv("AZURE_TENANT_ID")
	if clientID == "" || tenantID == "" {
		logger.Error(fmt.Errorf("missing identity env vars"), "❌ AZURE_CLIENT_ID or AZURE_TENANT_ID not set", "apiID", deployment.Spec.APIID)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = phaseError
			status.Status = phaseError
			status.Message = "AZURE_CLIENT_ID or AZURE_TENANT_ID not set"
			status.LastError = errMsgMissingAzureIdentity
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
			status.OpenAPIHash = openAPIHash
			status.DesiredHash = desiredHash
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	token, err := r.getToken.get(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token", "apiID", deployment.Spec.APIID)
		if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = phaseError
			status.Status = phaseError
			status.Message = errMsgFailedToGetAzureToken
			status.LastError = err.Error()
			status.LastAttemptAt = attemptTime
			status.ObservedGeneration = apimApi.Generation
			status.MatchedReplicaSets = matchedReplicaSetNames
			status.OpenAPIHash = openAPIHash
			status.DesiredHash = desiredHash
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	logger.Info("🔐 Obtained Azure AD token for APIM call", "apiID", deployment.Spec.APIID)

	// Step 3: Build the APIM deployment configuration with all necessary parameters.
	config := apim.APIMDeploymentConfig{
		Type:                 deployment.Spec.Type,
		SubscriptionID:       deployment.Spec.Subscription,
		ResourceGroup:        deployment.Spec.ResourceGroup,
		ServiceName:          deployment.Spec.APIMService,
		APIID:                deployment.Spec.APIID,
		RoutePrefix:          deployment.Spec.RoutePrefix,
		ServiceURL:           deployment.Spec.ServiceURL,
		Revision:             deployment.Spec.Revision,
		BearerToken:          token,
		ProductIDs:           deployment.Spec.ProductIDs,
		TagIDs:               deployment.Spec.TagIDs,
		SubscriptionRequired: deployment.Spec.SubscriptionRequired,
	}
	if ws := deployment.Spec.WebSocket; ws != nil {
		config.DisplayName = ws.DisplayName
		config.Protocols = ws.Protocols
	}
	logger.Info("🛠️ Built APIM deployment config",
		"apiID", config.APIID,
		"subscription", config.SubscriptionID,
		"resourceGroup", config.ResourceGroup,
		"serviceName", config.ServiceName,
		"routePrefix", config.RoutePrefix,
		"type", config.Type,
		"revision", config.Revision,
		"productCount", len(config.ProductIDs),
		"tagCount", len(config.TagIDs),
		"subscriptionRequired", config.SubscriptionRequired,
	)

	// Step 4: Create or update the API in Azure APIM. An HTTP API is imported from the
	// OpenAPI document; a websocket API is created from the spec alone. An import APIM
	// accepted earlier and may still be running is followed first: the API is never
	// written while APIM is still working on the previous write (see
	// apimapideployment_pending.go). An API already written for this desired state is not
	// written again: only the steps after it failed.
	upsertMessage := "Failed to import API into APIM"
	if isWebSocket {
		upsertMessage = "Failed to create WebSocket API in APIM"
	}
	writeAPI := deployment.Status.ImportedHash != desiredHash
	if pending != nil {
		outcome := checkPendingImport(ctx, pending, desiredHash, w.policy.Now(), func(ctx context.Context, operationURL string) (apim.OperationState, error) {
			return apim.GetOperationState(ctx, token, operationURL)
		})
		switch outcome.step {
		case pendingImportWait:
			logger.Info("⏳ "+outcome.message, "apiID", deployment.Spec.APIID, "detail", outcome.detail)
			if statusErr := updateAPIMAPIDeploymentStatus(ctx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
				status.Phase = apimDeploymentPhaseImporting
				status.Status = apimDeploymentStatusPending
				status.Message = outcome.message
				status.LastError = outcome.detail
				status.LastAttemptAt = attemptTime
				status.ObservedGeneration = apimApi.Generation
				status.MatchedReplicaSets = matchedReplicaSetNames
				status.OpenAPIHash = openAPIHash
				status.DesiredHash = desiredHash
				w.prepare(&status.RetryStatus)
			}); statusErr != nil {
				// Only the progress message is lost; the pending import is still recorded.
				logger.Error(statusErr, "❌ Failed to patch APIMAPIDeployment status", "apiID", deployment.Spec.APIID)
			}
			return ctrl.Result{RequeueAfter: pendingImportPollDelay(pending.StartedAt, w.policy.Now())}, nil
		case pendingImportFailed:
			// The write that failed is the import, whichever reconcile learns of it, and the
			// retry policy decides when it is made again. An import that ended any way but
			// finished for this desired state may have changed the API partway, so nothing
			// is known to be applied any more.
			logger.Error(outcome.err, "🚫 "+outcome.message, "apiID", deployment.Spec.APIID)
			return failWrite(upsertMessage, outcome.err, forgetPendingImport)
		case pendingImportDone:
			logger.Info("✅ "+outcome.message, "apiID", deployment.Spec.APIID)
			r.recordImported(ctx, logger, &deployment, desiredHash)
			writeAPI = false
		}
	}

	if writeAPI {
		// The write sets the API's path, backend serviceUrl and subscription requirement
		// together with its definition, so no separate patch of them follows.
		var written apim.WriteResult
		if isWebSocket {
			w.starting("step", "upsert websocket API")
			written, err = apim.UpsertWebSocketAPI(ctx, config)
		} else {
			w.starting("step", "import API", "bytes", len(openApiContent))
			written, err = apim.ImportOpenAPIDefinitionToAPIM(ctx, config, openApiContent)
		}
		if err != nil {
			logger.Error(err, "🚫 Failed to create or update API", "apiID", deployment.Spec.APIID, "type", config.Type)
			if errors.Is(err, apim.ErrWriteOutcomeUnknown) || errors.Is(err, apim.ErrNoOperationURL) {
				// APIM may be running it; do not send another on top of it soon.
				return failWriteNotBefore(unknownWriteRetryFloor, upsertMessage, err)
			}
			return failWrite(upsertMessage, err)
		}
		if written.Accepted() {
			// APIM runs the write in the background. Record it before anything else, and
			// follow it on later reconciles; the steps below run once it has finished.
			pendingImport, result, recorded := r.recordPendingImport(ctx, logger, &deployment, written, desiredHash, w.policy.Now())
			if recorded {
				return result, nil
			}
			// Try once more to keep the operation, in the failure's own patch, and keep the
			// next attempt well clear of the import APIM is running.
			return failWriteNotBefore(unknownWriteRetryFloor, upsertMessage, errPendingImportNotRecorded,
				func(status *apimv1.APIMAPIDeploymentStatus) { status.PendingImport = pendingImport })
		}
		logger.Info("✅ API created or updated in APIM", "apiID", deployment.Spec.APIID, "type", config.Type)
		r.recordImported(ctx, logger, &deployment, desiredHash)
	}

	// Step 5: Assign the API to all configured products (if any).
	// Products are used to group APIs and require subscriptions for access.
	if len(config.ProductIDs) > 0 {
		w.starting("step", "assign products", "productIDs", config.ProductIDs)
		if err := apim.AssignProductsToAPI(ctx, config); err != nil {
			logger.Error(err, "🚫 Failed to assign API to products", "apiID", deployment.Spec.APIID, "productIDs", config.ProductIDs)
			return failWrite("Failed to assign API to products", err)
		}
		logger.Info("✅ API assigned to products", "apiID", config.APIID, "productIDs", config.ProductIDs)
	} else {
		logger.Info("ℹ️ No product IDs configured; skipping product assignment", "apiID", deployment.Spec.APIID)
	}

	// Step 6: Assign the API to all configured tags (if any).
	// Tags help organize and categorize APIs for better management.
	if len(config.TagIDs) > 0 {
		w.starting("step", "assign tags", "tagIDs", config.TagIDs)
		if err := apim.AssignTagsToAPI(ctx, config); err != nil {
			logger.Error(err, "🚫 Failed to assign API to tags", "apiID", deployment.Spec.APIID, "tagIDs", config.TagIDs)
			return failWrite("Failed to assign API to tags", err)
		}
		logger.Info("✅ API assigned to tags", "apiID", config.APIID, "tagIDs", config.TagIDs)
	} else {
		logger.Info("ℹ️ No tag IDs configured; skipping tag assignment", "apiID", deployment.Spec.APIID)
	}

	// Step 7: Fetch APIM service host details and update the APIMAPI status.
	// This provides the full URLs for accessing the API through APIM.
	apiHost, developerPortalHost, err := apim.GetAPIMServiceDetails(ctx, config)
	if err != nil {
		logger.Error(err, "⚠️ Failed to fetch APIM details", "apiID", deployment.Spec.APIID)
		return failWrite("Failed to fetch APIM service details", err)
	}

	// Record the success on the deployment first: AppliedHash is what keeps the next
	// reconcile from writing the API again. A failing patch is logged and retried later,
	// never handed back as an error, which would put controller-runtime's rate limiter,
	// without any attempt limit, in charge of re-running every write above.
	outCtx, cancel := outcomeContext(ctx)
	defer cancel()
	if statusErr := updateAPIMAPIDeploymentStatus(outCtx, r.Client, &deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
		status.Phase = apimDeploymentPhaseSucceeded
		status.Status = "OK"
		status.Message = "Successfully reconciled API in APIM"
		status.LastError = ""
		status.LastAttemptAt = attemptTime
		status.ObservedGeneration = apimApi.Generation
		status.MatchedReplicaSets = matchedReplicaSetNames
		status.OpenAPIHash = openAPIHash
		status.DesiredHash = desiredHash
		status.AppliedHash = desiredHash
		status.ImportedHash = desiredHash
		status.ImportedAt = time.Now().UTC().Format(time.RFC3339)
		status.PendingImport = nil
		w.succeeded(&status.RetryStatus)
	}); statusErr != nil {
		logger.Error(statusErr, "❌ APIM is up to date but recording it failed; checking again later", "apiID", deployment.Spec.APIID)
		return ctrl.Result{RequeueAfter: requeueUnrecordedSuccess}, nil
	}

	// Then the APIMAPI, which the team's ArgoCD app reads. Best effort: the deployment
	// already records the outcome, and the next success or in-sync reconcile repairs it.
	apiScheme := "https"
	if isWebSocket {
		apiScheme = "wss"
	}
	r.setAPIMAPIStatus(outCtx, logger, &apimApi, apimAPIStatusOK, func(s *apimv1.APIMAPIStatus) {
		s.ImportedAt = time.Now().UTC().Format(time.RFC3339)
		s.ApiHost = fmt.Sprintf("%s://%s%s", apiScheme, apiHost, deployment.Spec.RoutePrefix)
		s.DeveloperPortalHost = fmt.Sprintf("https://%s", developerPortalHost)
	})
	logger.Info("📝 APIMAPI status patched after import",
		"name", apimApi.Name,
		"apiID", deployment.Spec.APIID,
		"apiHost", apimApi.Status.ApiHost,
		"developerPortalHost", apimApi.Status.DeveloperPortalHost,
		"subscriptionRequired", apimApi.Spec.SubscriptionRequired,
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *APIMAPIDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.apiReader == nil {
		r.apiReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&apimv1.APIMAPIDeployment{}).
		WithEventFilter(apimAPIDeploymentPredicate()).
		WithOptions(apimWriterOptions()).
		Named("apimapideployment").
		Complete(r)
}

func apimAPIDeploymentPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}

			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}

			oldAnnotations := e.ObjectOld.GetAnnotations()
			newAnnotations := e.ObjectNew.GetAnnotations()

			if oldAnnotations[apimDeploymentSignalAnnotation] != newAnnotations[apimDeploymentSignalAnnotation] {
				return true
			}

			if oldAnnotations[apimDeploymentReplicaSetAnnotation] != newAnnotations[apimDeploymentReplicaSetAnnotation] {
				return true
			}

			// apim.operator.io/retry lets a Stalled or Invalid deployment write again.
			if retryAnnotationChanged(e) {
				return true
			}

			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}
