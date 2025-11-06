package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

//+kubebuilder:rbac:groups=apim.hedinit.io,resources=replicasetwatchers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apim.hedinit.io,resources=replicasetwatchers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apim.hedinit.io,resources=replicasetwatchers/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
//+kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapideployments,verbs=get;list;watch;create;update;patch;delete

func (r *ReplicaSetWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("replicasetwatcher_controller")

	// logger.Info("🔁 Starting reconciliation", "replicaSet", req.Name)

	// Fetch the ReplicaSet that triggered this reconciliation.
	var rs appsv1.ReplicaSet
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		logger.Error(err, "❌ Failed to get ReplicaSet")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Extract the application name from the ReplicaSet labels.
	// The app.kubernetes.io/name label is used to match ReplicaSets with APIMAPI resources.
	appName := rs.Labels["app.kubernetes.io/name"]
	if appName == "" {
		// If no app label is found, skip processing this ReplicaSet.
		return ctrl.Result{}, nil
	}

	// Look up the associated APIMAPI resource for this application.
	// The APIMAPI resource contains the configuration for deploying to APIM.
	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: appName, Namespace: rs.Namespace}, &apimApi); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// If no APIMAPI resource exists, there's nothing to deploy.
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMAPI", "name", appName)
		return ctrl.Result{}, err
	}

	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		logger.Error(err, "❌ Failed to read operator namespace")
		return ctrl.Result{}, fmt.Errorf("read operator namespace: %w", err)
	}
	operatorNamespace := strings.TrimSpace(string(nsBytes))

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: apimApi.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		logger.Error(err, "❌ Failed to get APIMService", "name", apimApi.Spec.APIMService)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("🔗 Found APIMService", "name", apimService.Name)

	var existingRevision apimv1.APIMAPIDeployment
	err = r.Get(ctx, client.ObjectKey{Name: appName, Namespace: rs.Namespace}, &existingRevision)
	if err == nil {
		logger.Info("♻️ APIMAPIDeployment already exists, recreating", "name", appName)
		if err := r.Delete(ctx, &existingRevision); err != nil {
			logger.Error(err, "❌ Failed to delete existing APIMAPIDeployment", "name", appName)
			return ctrl.Result{}, err
		}
		// Wait briefly to avoid race condition with deletion
		time.Sleep(2 * time.Second)
	} else if !apierrors.IsNotFound(err) {
		logger.Error(err, "❌ Failed checking APIMAPIDeployment", "replicaSet", rs.Name)
		return ctrl.Result{}, err
	}

	// Check if there's at least one ready pod owned by this ReplicaSet.
	// We wait for a pod to be ready before triggering the APIM deployment
	// to ensure the application is actually running and can serve requests.
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(rs.Namespace)); err != nil {
		logger.Error(err, "❌ Failed listing Pods", "replicaSet", rs.Name)
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
	if ownerPod == nil {
		logger.Info("⏳ Waiting for Pod Ready", "replicaSet", rs.Name)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
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

	apiDeployment := &apimv1.APIMAPIDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: rs.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(&apimApi, schema.GroupVersionKind{
					Group:   "apim.hedinit.io",
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
			Subscription:         apimService.Spec.Subscription,
			ResourceGroup:        apimService.Spec.ResourceGroup,
			APIID:                apimApi.Spec.APIID,
			ProductIDs:           apimApi.Spec.ProductIDs,
			TagIDs:               apimApi.Spec.TagIDs,
		},
	}

	if err := r.Create(ctx, apiDeployment); err != nil {
		logger.Error(err, "❌ Failed to create APIMAPIDeployment")
	} else {
		logger.Info("📘 Created APIMAPIDeployment", "name", apiDeployment.Name)
	}

	return ctrl.Result{}, nil
}

func (r *ReplicaSetWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return true },
			UpdateFunc:  func(e event.UpdateEvent) bool { return false },
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
