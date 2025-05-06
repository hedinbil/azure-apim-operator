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

	netv1 "k8s.io/api/networking/v1"
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

	var ingressList netv1.IngressList
	if err := r.List(ctx, &ingressList, client.InNamespace(rs.Namespace)); err != nil {
		logger.Error(err, "❌ Unable to list ingresses")
		return ctrl.Result{}, err
	}

	for _, ing := range ingressList.Items {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil && path.Backend.Service.Name == appName {
					host := rule.Host
					swaggerPath := labels["apim.hedinit.io/swagger-path"]
					if swaggerPath == "" {
						swaggerPath = "/swagger/v1/swagger.json"
					}

					subscriptionID := labels["apim.hedinit.io/subscriptionid"]
					resourceGroup := labels["apim.hedinit.io/resourcegroup"]
					serviceName := labels["apim.hedinit.io/apim"]
					revision := labels["apim.hedinit.io/revision"]
					routePrefix := labels["apim.hedinit.io/routeprefix"]
					if routePrefix == "" {
						routePrefix = "/" + appName
					}

					apiObj := &apimv1.APIMAPIRevision{
						ObjectMeta: metav1.ObjectMeta{
							Name:      appName,
							Namespace: rs.Namespace,
							OwnerReferences: []metav1.OwnerReference{
								*metav1.NewControllerRef(&rs, schema.GroupVersionKind{
									Group:   "apps",
									Version: "v1",
									Kind:    "ReplicaSet",
								}),
							},
						},
						Spec: apimv1.APIMAPIRevisionSpec{
							Host:          host,
							RoutePrefix:   routePrefix,
							SwaggerPath:   swaggerPath,
							APIMService:   serviceName,
							Subscription:  subscriptionID,
							ResourceGroup: resourceGroup,
							Revision:      revision,
						},
					}

					if err := r.Create(ctx, apiObj); err != nil {
						logger.Error(err, "❌ Failed to create APIMAPIRevision object")
					} else {
						logger.Info("📘 APIMAPI created from ReplicaSet", "name", apiObj.Name)
					}
					return ctrl.Result{}, nil
				}
			}
		}
	}

	logger.Info("ℹ️ No matching ingress found for ReplicaSet")
	return ctrl.Result{}, nil
}

func (r *ReplicaSetWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		Named("replicasetwatcher").
		Complete(r)
}
