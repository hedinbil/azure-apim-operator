package controller

import (
	"context"

	apimv1 "github.com/hedinit/aks-apim-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	labels := rs.GetLabels()
	if labels["apim.hedinit.io/import"] != "true" {
		logger.Info("ℹ️ ReplicaSet does not have 'apim.hedinit.io/import=true', skipping")
		return ctrl.Result{}, nil
	}

	appName := labels["app.kubernetes.io/name"]
	if appName == "" {
		logger.Info("ℹ️ No 'app.kubernetes.io/name' label found, skipping")
		return ctrl.Result{}, nil
	}

	// Fetch the existing APIMAPI by app name
	var existing apimv1.APIMAPI
	if err := r.Get(ctx, client.ObjectKey{Name: appName, Namespace: rs.Namespace}, &existing); err != nil {
		logger.Error(err, "❌ Failed to get APIMAPI", "name", appName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	revision := labels["apim.hedinit.io/revision"]
	if revision == "" {
		revision = "default"
	}

	revisionObj := &apimv1.APIMAPIRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName + "-rev-" + revision,
			Namespace: rs.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(&existing, schema.GroupVersionKind{
					Group:   "apim.hedinit.io",
					Version: "v1",
					Kind:    "APIMAPI",
				}),
			},
		},
		Spec: apimv1.APIMAPIRevisionSpec{
			Host:          existing.Spec.Host,
			RoutePrefix:   existing.Spec.RoutePrefix,
			SwaggerPath:   existing.Spec.SwaggerPath,
			APIMService:   existing.Spec.APIMService,
			Subscription:  existing.Spec.Subscription,
			ResourceGroup: existing.Spec.ResourceGroup,
			APIID:         appName,
			Revision:      revision,
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
		Named("replicasetwatcher").
		Complete(r)
}
