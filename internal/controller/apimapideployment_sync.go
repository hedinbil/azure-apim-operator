package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	apimDeploymentPhaseWaitingForMatch    = "WaitingForMatch"
	apimDeploymentPhaseWaitingForReadyPod = "WaitingForReadyPod"
	apimDeploymentPhaseWaitingForRollout  = "WaitingForRollout"
	apimDeploymentPhaseImporting          = "Importing"
	apimDeploymentPhaseSucceeded          = "Succeeded"
	apimDeploymentStatusPending           = "Pending"
	apimDeploymentSignalAnnotation        = "apim.operator.io/replicaset-signal"
	apimDeploymentReplicaSetAnnotation    = "apim.operator.io/last-matched-replicaset"
	// apimDeploymentTargetAnnotation records the APIMAPI's workload selector the
	// deployment was last told about; see apimAPITargetSignature.
	apimDeploymentTargetAnnotation = "apim.operator.io/target"
)

type apimDeploymentHashInput struct {
	// Type is left empty for http APIs so every resource that predates the field keeps
	// the hash it already has applied; otherwise the upgrade would re-import all of them.
	Type                 string                   `json:"type,omitempty"`
	WebSocket            *apimv1.APIMAPIWebSocket `json:"websocket,omitempty"`
	APIID                string                   `json:"apiID"`
	APIMService          string                   `json:"apimService"`
	Subscription         string                   `json:"subscription"`
	ResourceGroup        string                   `json:"resourceGroup"`
	RoutePrefix          string                   `json:"routePrefix"`
	ServiceURL           string                   `json:"serviceUrl"`
	Revision             string                   `json:"revision"`
	SubscriptionRequired bool                     `json:"subscriptionRequired"`
	ProductIDs           []string                 `json:"productIds,omitempty"`
	TagIDs               []string                 `json:"tagIds,omitempty"`
	OpenAPIHash          string                   `json:"openApiHash"`
}

func ensureAPIMAPIDeployment(ctx context.Context, c client.Client, apimAPI *apimv1.APIMAPI) (*apimv1.APIMAPIDeployment, error) {
	key := client.ObjectKey{Name: apimAPI.Name, Namespace: apimAPI.Namespace}
	deployment := &apimv1.APIMAPIDeployment{}
	getErr := c.Get(ctx, key, deployment)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return nil, getErr
	}

	subscription, resourceGroup, err := resolveAPIMServiceLocation(ctx, c, apimAPI.Spec.APIMService, deployment.Spec.Subscription, deployment.Spec.ResourceGroup)
	if err != nil {
		return nil, err
	}

	desiredSpec := apimv1.APIMAPIDeploymentSpec{
		Type:                 apimAPI.Spec.Type,
		WebSocket:            apimAPI.Spec.WebSocket.DeepCopy(),
		ServiceURL:           apimAPI.Spec.ServiceURL,
		RoutePrefix:          apimAPI.Spec.RoutePrefix,
		OpenAPIDefinitionURL: apimAPI.Spec.OpenAPIDefinitionURL,
		ProductIDs:           append([]string(nil), apimAPI.Spec.ProductIDs...),
		TagIDs:               append([]string(nil), apimAPI.Spec.TagIDs...),
		APIMService:          apimAPI.Spec.APIMService,
		APIMAPIName:          apimAPI.Name,
		Subscription:         subscription,
		ResourceGroup:        resourceGroup,
		APIID:                apimAPI.Spec.APIID,
		SubscriptionRequired: apimAPI.Spec.SubscriptionRequired,
	}
	desiredOwnerReferences := []metav1.OwnerReference{*metav1.NewControllerRef(apimAPI, apimv1.GroupVersion.WithKind("APIMAPI"))}
	target := apimAPITargetSignature(apimAPI)

	if apierrors.IsNotFound(getErr) {
		deployment = &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:            apimAPI.Name,
				Namespace:       apimAPI.Namespace,
				OwnerReferences: desiredOwnerReferences,
				Annotations:     map[string]string{apimDeploymentTargetAnnotation: target},
			},
			Spec: desiredSpec,
		}
		if err := c.Create(ctx, deployment); err != nil {
			return nil, err
		}
		return deployment, nil
	}

	updated := deployment.DeepCopy()
	updated.Spec = desiredSpec
	updated.OwnerReferences = desiredOwnerReferences
	if updated.Annotations[apimDeploymentTargetAnnotation] != target {
		// The workload selector is not part of the deployment's spec, so changing it bumps
		// no generation; the signal annotation makes the deployment look for its pods again.
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		updated.Annotations[apimDeploymentTargetAnnotation] = target
		updated.Annotations[apimDeploymentSignalAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if equality.Semantic.DeepEqual(deployment.Spec, updated.Spec) &&
		equality.Semantic.DeepEqual(deployment.OwnerReferences, updated.OwnerReferences) &&
		equality.Semantic.DeepEqual(deployment.Annotations, updated.Annotations) {
		return deployment, nil
	}
	if err := c.Patch(ctx, updated, client.MergeFrom(deployment)); err != nil {
		return nil, err
	}
	deployment.Spec = updated.Spec
	deployment.OwnerReferences = updated.OwnerReferences
	deployment.Annotations = updated.Annotations
	return deployment, nil
}

// apimAPITargetSignature is the APIMAPI's workload selector in a comparable form; empty
// when it selects by the legacy app.kubernetes.io/name label.
func apimAPITargetSignature(apimAPI *apimv1.APIMAPI) string {
	if !hasAPIMAPITargetSelector(apimAPI) {
		return ""
	}
	encoded, err := json.Marshal(apimAPI.Spec.Target.Selector)
	if err != nil {
		return ""
	}
	return sha256Hex(encoded)
}

func touchAPIMAPIDeployment(ctx context.Context, c client.Client, deployment *apimv1.APIMAPIDeployment, replicaSetName string) error {
	updated := deployment.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[apimDeploymentSignalAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	updated.Annotations[apimDeploymentReplicaSetAnnotation] = replicaSetName
	if equality.Semantic.DeepEqual(deployment.Annotations, updated.Annotations) {
		return nil
	}
	if err := c.Patch(ctx, updated, client.MergeFrom(deployment)); err != nil {
		return err
	}
	deployment.Annotations = updated.Annotations
	return nil
}

func updateAPIMAPIDeploymentStatus(ctx context.Context, c client.Client, deployment *apimv1.APIMAPIDeployment, mutate func(*apimv1.APIMAPIDeploymentStatus)) error {
	updated := deployment.DeepCopy()
	mutate(&updated.Status)
	if equality.Semantic.DeepEqual(deployment.Status, updated.Status) {
		return nil
	}
	if err := c.Status().Patch(ctx, updated, client.MergeFrom(deployment)); err != nil {
		return err
	}
	deployment.Status = updated.Status
	return nil
}

// deploymentSpecChanged is the deployment's "spec changed" test for the retry gate:
// whether desiredHash, which covers the spec and the OpenAPI document fetched in this
// reconcile, differs from status.desiredHash in a way that should clear the failures.
//
// A change to anything but the document always counts. A change to the document alone
// counts unless the deployment is backing off. The document is fetched again on every
// reconcile, and one that is not byte-identical from fetch to fetch (two app versions
// behind one Service during a canary, a generator that embeds a timestamp) would
// otherwise clear the count on every attempt: the deployment would re-import every
// minute and never reach Stalled. During a backoff the next attempt imports whatever the
// document is by then, so nothing is lost by waiting. A new document still lifts a
// Stalled or Invalid deployment at once, which is how a fixed document recovers. Neither
// is requeued, so that only happens on an event (a rollout's ReplicaSet signal, an
// operator restart), and each such round is again bounded by five attempts.
func deploymentSpecChanged(spec *apimv1.APIMAPIDeploymentSpec, status *apimv1.APIMAPIDeploymentStatus,
	subscription, resourceGroup, desiredHash string) bool {
	if desiredHash == status.DesiredHash {
		return false
	}
	backingOff := status.ConsecutiveFailures > 0 && status.NextAttemptAt != ""
	if !backingOff || status.DesiredHash == "" {
		return true
	}
	// The desired hash with the document the failures were recorded against: equal to
	// status.desiredHash when only the document moved since.
	withPreviousDocument, err := buildDesiredAPIMStateHash(spec, subscription, resourceGroup, status.OpenAPIHash)
	return err != nil || withPreviousDocument != status.DesiredHash
}

func buildDesiredAPIMStateHash(spec *apimv1.APIMAPIDeploymentSpec, subscription string, resourceGroup string, openAPIHash string) (string, error) {
	productIDs := append([]string(nil), spec.ProductIDs...)
	tagIDs := append([]string(nil), spec.TagIDs...)
	sort.Strings(productIDs)
	sort.Strings(tagIDs)

	payload := apimDeploymentHashInput{
		APIID:                spec.APIID,
		APIMService:          spec.APIMService,
		Subscription:         subscription,
		ResourceGroup:        resourceGroup,
		RoutePrefix:          spec.RoutePrefix,
		ServiceURL:           spec.ServiceURL,
		Revision:             spec.Revision,
		SubscriptionRequired: spec.SubscriptionRequired,
		ProductIDs:           productIDs,
		TagIDs:               tagIDs,
		OpenAPIHash:          openAPIHash,
	}

	if spec.Type == apimv1.APITypeWebSocket {
		payload.Type = spec.Type
		payload.WebSocket = spec.WebSocket.DeepCopy()
		if payload.WebSocket != nil {
			sort.Strings(payload.WebSocket.Protocols)
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal desired APIM state: %w", err)
	}

	return sha256Hex(encoded), nil
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func findMatchingReplicaSetsForAPIMAPI(ctx context.Context, c client.Client, apimAPI *apimv1.APIMAPI) ([]appsv1.ReplicaSet, error) {
	var replicaSetList appsv1.ReplicaSetList
	if err := c.List(ctx, &replicaSetList, client.InNamespace(apimAPI.Namespace)); err != nil {
		return nil, err
	}

	matches := make([]appsv1.ReplicaSet, 0)
	for _, replicaSet := range replicaSetList.Items {
		if replicaSet.Spec.Replicas != nil && *replicaSet.Spec.Replicas == 0 {
			continue
		}
		matched, err := matchesReplicaSetAPIMAPI(&replicaSet, apimAPI)
		if err != nil {
			return nil, err
		}
		if matched {
			matches = append(matches, replicaSet)
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Name < matches[j].Name
	})

	return matches, nil
}

const (
	// requeueWaitingForWorkload is how often a deployment without a matching ReplicaSet or
	// a ready pod looks again, besides the ReplicaSet signals.
	requeueWaitingForWorkload = 2 * time.Minute
	// requeueWaitingForRollout is how often a deployment waiting out a rolling update
	// looks again. Nothing signals the end of one: the old ReplicaSet scaled to 0 is
	// ignored by the watcher.
	requeueWaitingForRollout = 30 * time.Second
)

// replicaSetRevisionAnnotation is the Deployment revision a ReplicaSet belongs to.
const replicaSetRevisionAnnotation = "deployment.kubernetes.io/revision"

// replicaSetsStillRollingOut returns the matched ReplicaSets that belong to an older
// revision of their Deployment than another matched one and still have ready pods: a
// rolling update that has not finished. ReplicaSets without an owner or a revision are
// never counted.
func replicaSetsStillRollingOut(replicaSets []appsv1.ReplicaSet) []string {
	newest := map[types.UID]int64{}
	revisions := make([]int64, len(replicaSets))
	owners := make([]types.UID, len(replicaSets))
	for i := range replicaSets {
		owner := metav1.GetControllerOf(&replicaSets[i])
		revision, err := strconv.ParseInt(replicaSets[i].Annotations[replicaSetRevisionAnnotation], 10, 64)
		if owner == nil || err != nil {
			revisions[i] = -1
			continue
		}
		owners[i], revisions[i] = owner.UID, revision
		if revision > newest[owner.UID] {
			newest[owner.UID] = revision
		}
	}
	var old []string
	for i := range replicaSets {
		if revisions[i] >= 0 && revisions[i] < newest[owners[i]] && replicaSets[i].Status.ReadyReplicas > 0 {
			old = append(old, replicaSets[i].Name)
		}
	}
	return old
}

func findReadyPodForReplicaSets(ctx context.Context, c client.Client, replicaSets []appsv1.ReplicaSet) (*corev1.Pod, error) {
	if len(replicaSets) == 0 {
		return nil, nil
	}

	replicaSetNames := make(map[string]struct{}, len(replicaSets))
	namespace := replicaSets[0].Namespace
	for _, replicaSet := range replicaSets {
		replicaSetNames[replicaSet.Name] = struct{}{}
	}

	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning || !isPodReady(&pod) {
			continue
		}
		for _, ref := range pod.OwnerReferences {
			if ref.Kind != "ReplicaSet" {
				continue
			}
			if _, ok := replicaSetNames[ref.Name]; ok {
				podCopy := pod
				return &podCopy, nil
			}
		}
	}

	return nil, nil
}

func matchesReplicaSetAPIMAPI(replicaSet *appsv1.ReplicaSet, apimAPI *apimv1.APIMAPI) (bool, error) {
	if hasAPIMAPITargetSelector(apimAPI) {
		return matchesAPIMAPITarget(apimAPI, replicaSet.Labels)
	}

	return replicaSet.Labels["app.kubernetes.io/name"] == apimAPI.Name, nil
}

func matchedReplicaSetNames(replicaSets []appsv1.ReplicaSet) []string {
	names := make([]string, 0, len(replicaSets))
	for _, replicaSet := range replicaSets {
		names = append(names, replicaSet.Name)
	}
	sort.Strings(names)
	return names
}

func resolveAPIMServiceLocation(ctx context.Context, c client.Client, apimServiceName string, currentSubscription string, currentResourceGroup string) (string, string, error) {
	operatorNamespace := getOperatorNamespace()

	var apimService apimv1.APIMService
	if err := c.Get(ctx, client.ObjectKey{Name: apimServiceName, Namespace: operatorNamespace}, &apimService); err != nil {
		if apierrors.IsNotFound(err) {
			return currentSubscription, currentResourceGroup, nil
		}
		return currentSubscription, currentResourceGroup, err
	}

	return apimService.Spec.Subscription, apimService.Spec.ResourceGroup, nil
}

// Values of APIMAPI status.status. The ArgoCD health check for APIMAPI reads them: OK is
// Healthy, Error is Degraded (libs/helm-charts/argo-cd/<version>/values-override-default.yaml
// in hedin-applications-state).
const (
	apimAPIStatusOK    = "OK"
	apimAPIStatusError = "Error"
)

// requeueUnrecordedSuccess is how long to wait before checking again when every APIM write
// succeeded but recording that in the deployment's status failed. The next reconcile finds
// the applied hash unchanged and writes the API again, so it waits well beyond a backoff
// step instead of retrying at the rate limiter's pace.
const requeueUnrecordedSuccess = 5 * time.Minute

// errPendingImportNotRecorded is the write failure counted when APIM accepted an import but
// the status could not keep its operation (see recordPendingImport). The import may still be
// running; the retry policy's backoff decides when to try again.
var errPendingImportNotRecorded = errors.New("APIM accepted the import, but its operation could not be recorded in status.pendingImport")

// setAPIMAPIStatus sets an APIMAPI's status.status, and whatever else also changes, in one
// merge patch. Best effort: a failure is logged, since the APIMAPIDeployment already records
// the outcome and the next success or in-sync reconcile writes it again.
func (r *APIMAPIDeploymentReconciler) setAPIMAPIStatus(
	ctx context.Context,
	logger logr.Logger,
	apimAPI *apimv1.APIMAPI,
	value string,
	also ...func(*apimv1.APIMAPIStatus),
) {
	base := apimAPI.DeepCopy()
	apimAPI.Status.Status = value
	for _, mutate := range also {
		mutate(&apimAPI.Status)
	}
	if equality.Semantic.DeepEqual(base.Status, apimAPI.Status) {
		return
	}
	if err := r.Status().Patch(ctx, apimAPI, client.MergeFrom(base)); err != nil {
		logger.Error(err, "⚠️ Failed to patch APIMAPI status", "apimapi", apimAPI.Name, "status", value)
	}
}
