package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	apimv1 "github.com/hedinit/aks-apim-operator/api/v1"
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

// ReplicaSetWatcherReconciler reconciles a ReplicaSetWatcher object
type ReplicaSetWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=replicasetwatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=replicasetwatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=replicasetwatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapirevisions,verbs=get;list;watch;create;update;patch;delete

func (r *ReplicaSetWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("replicasetwatcher_controller")

	var rs appsv1.ReplicaSet
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		logger.Error(err, "❌ Failed to get ReplicaSet")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// if !rs.DeletionTimestamp.IsZero() {
	// 	logger.Info("🧹 ReplicaSet is being deleted, skipping", "name", rs.Name)
	// 	return ctrl.Result{}, nil
	// }

	labels := rs.GetLabels()
	appName := labels["app.kubernetes.io/name"]
	if appName == "" {
		logger.Info("ℹ️ No 'app.kubernetes.io/name' label found, skipping")
		return ctrl.Result{}, nil
	}

	var apimApi apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: appName, Namespace: rs.Namespace}, &apimApi); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("ℹ️ APIMAPI not found, skipping revision creation", "name", appName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "❌ Failed to get APIMAPI", "name", appName)
		return ctrl.Result{}, err
	}

	operatorNamespaceBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		logger.Error(err, "❌ Unable to determine operator namespace")
		return ctrl.Result{}, fmt.Errorf("failed to read operator namespace: %w", err)
	}
	operatorNamespace := strings.TrimSpace(string(operatorNamespaceBytes))

	var apimService apimv1.APIMService
	if err := r.Get(ctx, client.ObjectKey{Name: apimApi.Spec.APIMService, Namespace: operatorNamespace}, &apimService); err != nil {
		logger.Error(err, "❌ Failed to fetch referenced APIMService", "name", apimApi.Spec.APIMService)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("🔗 Found APIMService", "name", apimService.Name)

	logger.Info("🔍 Processing ReplicaSet", "name", rs.Name)

	revisionName := appName + "-deployment"

	var existingRevision apimv1.APIMAPIRevision
	err = r.Get(ctx, client.ObjectKey{Name: revisionName, Namespace: rs.Namespace}, &existingRevision)
	if err == nil {
		logger.Info("ℹ️ APIMAPIRevision already exists, skipping creation", "name", revisionName)
		return ctrl.Result{}, nil
	}
	if !apierrors.IsNotFound(err) {
		logger.Error(err, "❌ Failed to check existence of APIMAPIRevision")
		return ctrl.Result{}, err
	}

	// Find pod owned by this ReplicaSet
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(rs.Namespace)); err != nil {
		logger.Error(err, "❌ Failed to list pods in namespace")
		return ctrl.Result{}, err
	}

	// Find a pod owned by this ReplicaSet that is Running and Ready
	var ownerPod *corev1.Pod
	for _, pod := range podList.Items {
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "ReplicaSet" && ref.Name == rs.Name {
				if pod.Status.Phase == corev1.PodRunning && isPodReady(&pod) {
					ownerPod = &pod
					break
				}
			}
		}
		if ownerPod != nil {
			break
		}
	}

	if ownerPod == nil {
		logger.Info("⏳ No ready pod found for ReplicaSet yet, requeuing...", "replicaSet", rs.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	revisionObj := &apimv1.APIMAPIRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionName,
			Namespace: rs.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(&apimApi, schema.GroupVersionKind{
					Group:   "apim.hedinit.io",
					Version: "v1",
					Kind:    "APIMAPI",
				}),
				// {
				// 	APIVersion: corev1.SchemeGroupVersion.String(),
				// 	Kind:       "Pod",
				// 	Name:       ownerPod.Name,
				// 	UID:        ownerPod.UID,
				// 	// Controller and BlockOwnerDeletion must only be true for *one* owner
				// 	Controller:         pointer(false),
				// 	BlockOwnerDeletion: pointer(true),
				// },
			},
		},
		Spec: apimv1.APIMAPIRevisionSpec{
			Host:          apimApi.Spec.Host,
			RoutePrefix:   apimApi.Spec.RoutePrefix,
			SwaggerPath:   apimApi.Spec.SwaggerPath,
			APIMService:   apimApi.Spec.APIMService,
			Subscription:  apimService.Spec.Subscription,
			ResourceGroup: apimService.Spec.ResourceGroup,
			APIID:         appName,
			Revision:      "",
		},
	}

	if err := r.Create(ctx, revisionObj); err != nil {
		logger.Error(err, "❌ Failed to create APIMAPIRevision object")
	} else {
		logger.Info("📘 APIMAPIRevision created", "name", revisionObj.Name)
	}

	return ctrl.Result{}, nil
}

func (r *ReplicaSetWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		//Owns(&corev1.Pod{}). // ← this adds a watch on child Pods
		Named("replicasetwatcher").
		Complete(r)
}

func pointer[T any](v T) *T {
	return &v
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
