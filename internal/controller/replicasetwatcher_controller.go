package controller

import (
	"context"
	"fmt"
	"time"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// +kubebuilder:rbac:groups=apim.operator.io,resources=replicasetwatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=replicasetwatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.operator.io,resources=replicasetwatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.operator.io,resources=apimapideployments,verbs=get;list;watch;create;update;patch;delete

func (r *ReplicaSetWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("replicasetwatcher_controller")

	// logger.Info("🔁 Starting reconciliation", "replicaSet", req.Name)

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

	operatorNamespace, err := getOperatorNamespace()
	if err != nil {
		logger.Error(err, "❌ Failed to get operator namespace", "replicaSet", rs.Name, "namespace", rs.Namespace)
		return ctrl.Result{}, fmt.Errorf("get operator namespace: %w", err)
	}

	// Check if there's at least one ready pod owned by this ReplicaSet.
	// We wait for a pod to be ready before triggering the APIM deployment
	// to ensure the application is actually running and can serve requests.
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(rs.Namespace)); err != nil {
		logger.Error(err, "❌ Failed listing Pods", "replicaSet", rs.Name, "namespace", rs.Namespace)
		return ctrl.Result{}, err
	}

	// Find a pod owned by this ReplicaSet that is running and ready.
	var ownerPod *corev1.Pod
	for _, pod := range podList.Items {
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "ReplicaSet" && ref.Name == rs.Name &&
				pod.Status.Phase == corev1.PodRunning && isPodReady(&pod) {
				ownerPod = &pod
				break
			}
		}
		if ownerPod != nil {
			break
		}
	}
	// If no ready pod is found, requeue to wait for the pod to become ready.
	// Use a longer interval to reduce log spam, and rely on ReplicaSet status updates
	// to trigger reconciliation when pods become ready.
	if ownerPod == nil {
		logger.Info("⏳ Waiting for Pod Ready", "replicaSet", rs.Name, "namespace", rs.Namespace, "readyReplicas", rs.Status.ReadyReplicas, "replicas", rs.Status.Replicas, "matchCount", len(apimApis))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// var ingressList networkingv1.IngressList
	// if err := r.List(ctx, &ingressList, client.InNamespace(rs.Namespace)); err != nil {
	// 	logger.Error(err, "❌ Failed to list Ingresses")
	// 	return ctrl.Result{}, err
	// }

	// var matchingIngress *networkingv1.Ingress
	// for _, ing := range ingressList.Items {
	// 	for _, rule := range ing.Spec.Rules {
	// 		if rule.Host == apimApi.Spec.Host {
	// 			matchingIngress = &ing
	// 			break
	// 		}
	// 	}
	// 	if matchingIngress != nil {
	// 		break
	// 	}
	// }
	// if matchingIngress == nil {
	// 	logger.Info("⏳ No matching Ingress yet", "host", apimApi.Spec.Host)
	// 	return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	// }

	// logger.Info("🌐 Found matching Ingress", "ingress", matchingIngress.Name)

	var reconcileErrs []error
	for _, apimApi := range apimApis {
		var apimService apimv1.APIMService
		if err := r.Get(ctx, client.ObjectKey{Name: apimApi.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
			logger.Error(err, "❌ Failed to get APIMService", "name", apimApi.Spec.APIMService, "apiID", apimApi.Spec.APIID, "apimapi", apimApi.Name)
			reconcileErrs = append(reconcileErrs, client.IgnoreNotFound(err))
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

		var existingRevision apimv1.APIMAPIDeployment
		err = r.Get(ctx, client.ObjectKey{Name: apimApi.Name, Namespace: rs.Namespace}, &existingRevision)
		if err == nil {
			logger.Info("♻️ APIMAPIDeployment already exists, deleting to get latest swagger", "name", apimApi.Name, "apiID", apimApi.Spec.APIID)
			if err := r.Delete(ctx, &existingRevision); err != nil {
				logger.Error(err, "❌ Failed to delete existing APIMAPIDeployment", "name", apimApi.Name, "apiID", apimApi.Spec.APIID)
				reconcileErrs = append(reconcileErrs, err)
				continue
			}
			// Wait briefly to avoid race condition with deletion
			time.Sleep(2 * time.Second)
		} else if !apierrors.IsNotFound(err) {
			logger.Error(err, "❌ Failed checking APIMAPIDeployment", "replicaSet", rs.Name, "apiID", apimApi.Spec.APIID)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}

		apiDeployment := &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      apimApi.Name,
				Namespace: rs.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(&apimApi, schema.GroupVersionKind{
						Group:   "apim.operator.io",
						Version: "v1",
						Kind:    "APIMAPI",
					}),
				},
			},
			Spec: apimv1.APIMAPIDeploymentSpec{
				ServiceURL:           apimApi.Spec.ServiceURL,
				RoutePrefix:          apimApi.Spec.RoutePrefix,
				OpenAPIDefinitionURL: apimApi.Spec.OpenAPIDefinitionURL,
				APIMService:          apimApi.Spec.APIMService,
				APIMAPIName:          apimApi.Name,
				Subscription:         apimService.Spec.Subscription,
				ResourceGroup:        apimService.Spec.ResourceGroup,
				APIID:                apimApi.Spec.APIID,
				ProductIDs:           apimApi.Spec.ProductIDs,
				TagIDs:               apimApi.Spec.TagIDs,
				SubscriptionRequired: apimApi.Spec.SubscriptionRequired,
			},
		}

		if err := r.Create(ctx, apiDeployment); err != nil {
			logger.Error(err, "❌ Failed to create APIMAPIDeployment", "name", apiDeployment.Name, "apiID", apimApi.Spec.APIID)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}

		logger.Info("📘 Created APIMAPIDeployment", "name", apiDeployment.Name, "apiID", apimApi.Spec.APIID, "apimApiName", apiDeployment.Spec.APIMAPIName)
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
				// Only reconcile on ReplicaSet updates when the status changes
				// Specifically, when ReadyReplicas changes, indicating pods becoming ready
				oldRS, ok := e.ObjectOld.(*appsv1.ReplicaSet)
				if !ok {
					return false
				}
				newRS, ok := e.ObjectNew.(*appsv1.ReplicaSet)
				if !ok {
					return false
				}

				// Skip updates on old ReplicaSet revisions that have been scaled down to 0.
				// When a Deployment is updated, the old ReplicaSet is scaled down to 0,
				// and we should not process these old revisions even if ReadyReplicas changes.
				if newRS.Spec.Replicas != nil && *newRS.Spec.Replicas == 0 {
					return false
				}

				// Reconcile only when ReadyReplicas changes from 0 to greater than 0.
				// This ensures we only trigger APIM deployments when pods actually become ready,
				// not when they decrease or change in other ways.
				// This handles the case where a ReplicaSet is created with ReadyReplicas = 0,
				// and then pods become ready later.
				return oldRS.Status.ReadyReplicas == 0 && newRS.Status.ReadyReplicas > 0
			},
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		Named("replicasetwatcher").
		Complete(r)
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

// func getLoggerWithTrace(ctx context.Context) *zap.Logger {
// 	base := zap.New(zap.UseDevMode(true)) // or zap.NewProduction() for prod
// 	span := trace.SpanFromContext(ctx)
// 	sc := span.SpanContext()
// 	if sc.IsValid() {
// 		return base.With(
// 			zap.String("trace_id", sc.TraceID().String()),
// 			zap.String("span_id", sc.SpanID().String()),
// 		)
// 	}
// 	return base
// }
