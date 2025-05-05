package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
)

// PodWatcherReconciler reconciles a PodWatcher object
type PodWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=podwatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=podwatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=podwatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PodWatcher object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *PodWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("podwatcher_controller")

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		logger.Error(err, "❌ Unable to fetch Pod")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	labels := pod.GetLabels()
	if labels["apim.hedinit.io/import"] != "true" {
		return ctrl.Result{}, nil
	}

	appName := labels["app"]
	if appName == "" {
		logger.Info("ℹ️ No 'app' label found on pod, skipping")
		return ctrl.Result{}, nil
	}

	// Find matching ingress
	var ingressList netv1.IngressList
	if err := r.List(ctx, &ingressList, client.InNamespace(pod.Namespace)); err != nil {
		logger.Error(err, "❌ Unable to list ingresses")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Named("podwatcher").
		Complete(r)
}
