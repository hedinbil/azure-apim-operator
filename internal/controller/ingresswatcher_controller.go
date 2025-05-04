package controller

import (
	"context"
	"time"

	apimv1 "github.com/hedinit/aks-apim-operator/api/v1"
	v1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IngressWatcherReconciler reconciles an IngressWatcher object
type IngressWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apim.hedinit.io,resources=ingresswatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=ingresswatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=ingresswatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apim.hedinit.io,resources=apimapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch

func (r *IngressWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var logger = ctrl.Log.WithName("ingresswatcher_controller")

	var ingress v1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		logger.Error(err, "❌ Unable to fetch Ingress")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	annotations := ingress.Annotations
	logger.Info("🔍 Ingress detected for reconciliation",
		"name", ingress.Name,
		"namespace", ingress.Namespace,
		"annotations", annotations,
	)

	if annotations["apim.hedinit.io/import"] != "true" {
		logger.Info("⛔ Skipping APIM import – annotation not set or false")
		return ctrl.Result{}, nil
	}

	var host string
	if len(ingress.Spec.Rules) > 0 && ingress.Spec.Rules[0].Host != "" {
		host = ingress.Spec.Rules[0].Host
	} else if len(ingress.Status.LoadBalancer.Ingress) > 0 {
		lb := ingress.Status.LoadBalancer.Ingress[0]
		if lb.Hostname != "" {
			host = lb.Hostname
		} else if lb.IP != "" {
			host = lb.IP
		}
	}

	if host == "" {
		logger.Info("⏳ Could not determine Ingress host, will retry")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	swaggerPath := annotations["apim.hedinit.io/swagger-path"]
	if swaggerPath == "" {
		swaggerPath = "/swagger.yaml"
	}

	subscriptionID := annotations["apim.hedinit.io/subscriptionid"]
	resourceGroup := annotations["apim.hedinit.io/resourcegroup"]
	serviceName := annotations["apim.hedinit.io/apim"]
	routePrefix := annotations["apim.hedinit.io/routeprefix"]
	if routePrefix == "" {
		routePrefix = "/" + ingress.Name
	}

	apiObj := &apimv1.APIMAPI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingress.Name,
			Namespace: ingress.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(&ingress, schema.GroupVersionKind{
					Group:   "networking.k8s.io",
					Version: "v1",
					Kind:    "Ingress",
				}),
			},
		},
		Spec: apimv1.APIMAPISpec{
			Host:          host,
			RoutePrefix:   routePrefix,
			SwaggerPath:   swaggerPath,
			APIMService:   serviceName,
			Subscription:  subscriptionID,
			ResourceGroup: resourceGroup,
		},
	}

	if err := r.Create(ctx, apiObj); err != nil {
		logger.Error(err, "❌ Failed to create APIMAPI object")
	} else {
		logger.Info("📘 APIMAPI created (to be handled by APIMAPI controller)", "name", apiObj.Name)
	}

	return ctrl.Result{}, nil
}

func (r *IngressWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Ingress{}).
		Named("ingresswatcher").
		Complete(r)
}
