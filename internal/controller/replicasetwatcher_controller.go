package controller

import (
	"context"
	"fmt"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ReplicaSetWatcherReconciler watches Kubernetes ReplicaSet resources and triggers
// APIM API deployments when new replicas become ready. This controller enables
// automatic API deployment to Azure APIM when applications are deployed or updated
// in the cluster. It creates APIMAPIDeployment resources based on associated APIMAPI
// custom resources.
type ReplicaSetWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments,verbs=get;list;watch;create;update;patch;delete

func (r *ReplicaSetWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("replicasetwatcher_controller")

	// Fetch the ReplicaSet that triggered this reconciliation.
	var rs appsv1.ReplicaSet
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		logger.Error(err, "❌ Failed to get ReplicaSet")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip old ReplicaSet revisions that have been scaled down to 0 replicas.
	// When a Deployment is updated, the old ReplicaSet is scaled down to 0,
	// and we should not process these old revisions.
	if rs.Spec.Replicas != nil && *rs.Spec.Replicas == 0 {
		logger.Info("⏭️ Skipping old ReplicaSet revision (scaled down to 0)",
			"replicaSet", rs.Name, "namespace", rs.Namespace)
		return ctrl.Result{}, nil
	}

	// Extract the legacy application name from the ReplicaSet labels.
	// This is still used as a fallback when APIMAPI.spec.target.selector is not set.
	appName := rs.Labels["app.kubernetes.io/name"]

	apimApis, err := r.findMatchingAPIMAPIs(ctx, &rs)
	if err != nil {
		logger.Error(err, "❌ Failed to resolve APIMAPI targets", "replicaSet", rs.Name, "namespace", rs.Namespace, "appName", appName)
		return ctrl.Result{}, err
	}
	if len(apimApis) == 0 {
		logger.Info("ℹ️ No matching APIMAPI resources for ReplicaSet; skipping deployment",
			"replicaSet", rs.Name,
			"namespace", rs.Namespace,
			"appName", appName)
		return ctrl.Result{}, nil
	}

	logger.Info("🎯 Matched APIMAPI resources for ReplicaSet",
		"replicaSet", rs.Name,
		"namespace", rs.Namespace,
		"appName", appName,
		"matchCount", len(apimApis),
	)

	var reconcileErrs []error
	for _, apimApi := range apimApis {
		apiDeployment, err := ensureAPIMAPIDeployment(ctx, r.Client, &apimApi)
		if err != nil {
			logger.Error(err, "❌ Failed to ensure APIMAPIDeployment", "apimapi", apimApi.Name, "apiID", apimApi.Spec.APIID)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}

		logger.Info("🚀 Preparing APIM deployment",
			"replicaSet", rs.Name,
			"namespace", rs.Namespace,
			"apimapi", apimApi.Name,
			"apiID", apimApi.Spec.APIID,
			"routePrefix", apimApi.Spec.RoutePrefix,
			"openApiUrl", apimApi.Spec.OpenAPIDefinitionURL,
			"productCount", len(apimApi.Spec.ProductIDs),
			"tagCount", len(apimApi.Spec.TagIDs),
			"subscriptionRequired", apimApi.Spec.SubscriptionRequired,
		)

		if err := touchAPIMAPIDeployment(ctx, r.Client, apiDeployment, rs.Name); err != nil {
			logger.Error(err, "❌ Failed to signal APIMAPIDeployment", "name", apiDeployment.Name, "apiID", apimApi.Spec.APIID)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}

		logger.Info("📣 Signaled APIMAPIDeployment", "name", apiDeployment.Name, "apiID", apimApi.Spec.APIID, "apimApiName", apiDeployment.Spec.APIMAPIName)
	}

	if len(reconcileErrs) > 0 {
		return ctrl.Result{}, utilerrors.NewAggregate(reconcileErrs)
	}

	return ctrl.Result{}, nil
}

func (r *ReplicaSetWatcherReconciler) findMatchingAPIMAPIs(ctx context.Context, rs *appsv1.ReplicaSet) ([]apimv1.APIMAPI, error) {
	logger := ctrl.Log.WithName("replicasetwatcher_controller")
	matches := make([]apimv1.APIMAPI, 0)

	var apimApiList apimv1.APIMAPIList
	if err := r.List(ctx, &apimApiList, client.InNamespace(rs.Namespace)); err != nil {
		return nil, err
	}

	for _, apimApi := range apimApiList.Items {
		if !hasAPIMAPITargetSelector(&apimApi) {
			continue
		}

		matched, err := matchesAPIMAPITarget(&apimApi, rs.Labels)
		if err != nil {
			logger.Error(err, "❌ Invalid APIMAPI target selector; skipping", "apimapi", apimApi.Name, "namespace", apimApi.Namespace)
			continue
		}
		if !matched {
			continue
		}

		matches = appendUniqueAPIMAPI(matches, apimApi)
	}

	appName := rs.Labels["app.kubernetes.io/name"]
	if appName == "" {
		return matches, nil
	}

	var legacyAPIMAPI apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: appName, Namespace: rs.Namespace}, &legacyAPIMAPI); err != nil {
		if apierrors.IsNotFound(err) {
			return matches, nil
		}
		return nil, err
	}
	if hasAPIMAPITargetSelector(&legacyAPIMAPI) {
		return matches, nil
	}

	return appendUniqueAPIMAPI(matches, legacyAPIMAPI), nil
}

func hasAPIMAPITargetSelector(apimApi *apimv1.APIMAPI) bool {
	return apimApi.Spec.Target != nil && apimApi.Spec.Target.Selector != nil
}

func matchesAPIMAPITarget(apimApi *apimv1.APIMAPI, rsLabels map[string]string) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(apimApi.Spec.Target.Selector)
	if err != nil {
		return false, fmt.Errorf("target selector for APIMAPI %s: %w", apimApi.Name, err)
	}

	return selector.Matches(labels.Set(rsLabels)), nil
}

func appendUniqueAPIMAPI(matches []apimv1.APIMAPI, apimApi apimv1.APIMAPI) []apimv1.APIMAPI {
	for _, existing := range matches {
		if existing.Name == apimApi.Name && existing.Namespace == apimApi.Namespace {
			return matches
		}
	}

	return append(matches, apimApi)
}

func (r *ReplicaSetWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				// Skip creates for old ReplicaSet revisions that have been scaled down to 0.
				// When a Deployment is updated, old ReplicaSets may be recreated or watched
				// with 0 replicas, and we should not process these old revisions.
				rs, ok := e.Object.(*appsv1.ReplicaSet)
				if !ok {
					return false
				}
				if rs.Spec.Replicas != nil && *rs.Spec.Replicas == 0 {
					return false
				}
				// Only process Create events if pods are already ready.
				// If pods aren't ready yet, we'll wait for the Update event when ReadyReplicas
				// changes from 0 to >0. This prevents duplicate deployments from both Create
				// and Update events when a ReplicaSet is created with ready pods.
				return rs.Status.ReadyReplicas > 0
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldRS, ok := e.ObjectOld.(*appsv1.ReplicaSet)
				if !ok {
					return false
				}
				newRS, ok := e.ObjectNew.(*appsv1.ReplicaSet)
				if !ok {
					return false
				}
				return replicaSetReadinessSignal(oldRS, newRS)
			},
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		Named("replicasetwatcher").
		Complete(r)
}

// replicaSetReadinessSignal reports whether an update of a ReplicaSet should make its APIs
// deploy: its first pod became ready, or all the pods it wants became ready.
//
// The first is the earliest the new version can answer; the second is usually the moment
// a rolling update ends. The deployment controller does not fetch the document while an
// older revision still has ready pods (replicaSetsStillRollingOut), because the OpenAPI URL,
// a Service, would still reach them; it looks again on its own until they are gone. A
// ReplicaSet scaled down to 0, the old revision of a rollout, never signals.
func replicaSetReadinessSignal(oldRS, newRS *appsv1.ReplicaSet) bool {
	want := int32(1)
	if newRS.Spec.Replicas != nil {
		want = *newRS.Spec.Replicas
	}
	if want == 0 {
		return false
	}
	firstReady := oldRS.Status.ReadyReplicas == 0 && newRS.Status.ReadyReplicas > 0
	allReady := oldRS.Status.ReadyReplicas < want && newRS.Status.ReadyReplicas >= want
	return firstReady || allReady
}

// isPodReady checks if a pod is in the Ready condition.
// A pod is ready when all its containers are running and passing readiness probes.
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
