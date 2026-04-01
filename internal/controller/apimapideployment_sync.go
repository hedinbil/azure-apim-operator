package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	apimDeploymentPhaseWaitingForMatch    = "WaitingForMatch"
	apimDeploymentPhaseWaitingForReadyPod = "WaitingForReadyPod"
	apimDeploymentPhaseImporting          = "Importing"
	apimDeploymentPhaseSucceeded          = "Succeeded"
	apimDeploymentStatusPending           = "Pending"
	apimDeploymentSignalAnnotation        = "apim.operator.io/replicaset-signal"
	apimDeploymentReplicaSetAnnotation    = "apim.operator.io/last-matched-replicaset"
)

type apimDeploymentHashInput struct {
	APIID                string   `json:"apiID"`
	APIMService          string   `json:"apimService"`
	Subscription         string   `json:"subscription"`
	ResourceGroup        string   `json:"resourceGroup"`
	RoutePrefix          string   `json:"routePrefix"`
	ServiceURL           string   `json:"serviceUrl"`
	Revision             string   `json:"revision"`
	SubscriptionRequired bool     `json:"subscriptionRequired"`
	ProductIDs           []string `json:"productIds,omitempty"`
	TagIDs               []string `json:"tagIds,omitempty"`
	OpenAPIHash          string   `json:"openApiHash"`
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

	if apierrors.IsNotFound(getErr) {
		deployment = &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:            apimAPI.Name,
				Namespace:       apimAPI.Namespace,
				OwnerReferences: desiredOwnerReferences,
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
	if equality.Semantic.DeepEqual(deployment.Spec, updated.Spec) && equality.Semantic.DeepEqual(deployment.OwnerReferences, updated.OwnerReferences) {
		return deployment, nil
	}
	if err := c.Patch(ctx, updated, client.MergeFrom(deployment)); err != nil {
		return nil, err
	}
	deployment.Spec = updated.Spec
	deployment.OwnerReferences = updated.OwnerReferences
	return deployment, nil
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
	operatorNamespace, err := getOperatorNamespace()
	if err != nil {
		return currentSubscription, currentResourceGroup, err
	}

	var apimService apimv1.APIMService
	if err := c.Get(ctx, client.ObjectKey{Name: apimServiceName, Namespace: operatorNamespace}, &apimService); err != nil {
		if apierrors.IsNotFound(err) {
			return currentSubscription, currentResourceGroup, nil
		}
		return currentSubscription, currentResourceGroup, err
	}

	return apimService.Spec.Subscription, apimService.Spec.ResourceGroup, nil
}
