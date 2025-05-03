/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	apim "github.com/hedinit/aks-apim-operator/internal/apim"
	identity "github.com/hedinit/aks-apim-operator/internal/identity"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IngressWatcherReconciler reconciles a IngressWatcher object
type IngressWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

var logger = ctrl.Log.WithName("controller")

// +kubebuilder:rbac:groups=net.hedinit.io,resources=ingresswatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=net.hedinit.io,resources=ingresswatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=net.hedinit.io,resources=ingresswatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the IngressWatcher object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *IngressWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ingress networkingv1.Ingress
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

	if annotations["apim.hedinit.io/import"] != "true" {
		logger.Info("⛔ Skipping APIM import – annotation not set or false")
		return ctrl.Result{}, nil
	}

	swaggerPath := annotations["apim.hedinit.io/swagger-path"]
	if swaggerPath == "" {
		swaggerPath = "/swagger.yaml"
	}
	swaggerURL := fmt.Sprintf("https://%s%s", host, swaggerPath)
	logger.Info("📡 Fetching Swagger document", "url", swaggerURL)

	resp, err := http.Get(swaggerURL)
	if err != nil {
		logger.Error(err, "❌ Failed to fetch Swagger document")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer resp.Body.Close()

	swaggerYAML, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(err, "❌ Failed to read Swagger body")
		return ctrl.Result{}, err
	}

	logger.Info("📄 Swagger fetched successfully", "status", resp.StatusCode)

	// Extract required metadata from annotations
	subscriptionID := annotations["apim.hedinit.io/subscriptionid"]
	resourceGroup := annotations["apim.hedinit.io/resourcegroup"]
	serviceName := annotations["apim.hedinit.io/apim"]
	routePrefix := annotations["apim.hedinit.io/routeprefix"]
	if routePrefix == "" {
		routePrefix = "/" + ingress.Name
	}

	logger.Info("🔧 APIM configuration extracted",
		"subscriptionID", subscriptionID,
		"resourceGroup", resourceGroup,
		"serviceName", serviceName,
		"apiID", ingress.Name,
		"routePrefix", routePrefix,
	)

	clientID := os.Getenv("AZURE_CLIENT_ID")
	if clientID == "" {
		logger.Error(fmt.Errorf("missing AZURE_CLIENT_ID"), "❌ Environment variable not set")
		return ctrl.Result{}, fmt.Errorf("AZURE_CLIENT_ID not set")
	}

	tenantID := os.Getenv("AZURE_TENANT_ID")
	if tenantID == "" {
		logger.Error(fmt.Errorf("missing AZURE_TENANT_ID"), "❌ Environment variable not set")
		return ctrl.Result{}, fmt.Errorf("AZURE_TENANT_ID not set")
	}

	token, err := identity.GetManagementToken(ctx, clientID, tenantID)
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure management token")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	err = apim.ImportSwaggerToAPIM(ctx, apim.APIMConfig{
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ServiceName:    serviceName,
		APIID:          ingress.Name,
		RoutePrefix:    routePrefix,
		ServiceURL:     fmt.Sprintf("https://%s", host),
		BearerToken:    token,
	}, swaggerYAML)
	if err != nil {
		logger.Error(err, "🚫 Failed to import API into APIM")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	logger.Info("✅ Successfully imported API into APIM",
		"apiID", ingress.Name,
		"serviceUrl", fmt.Sprintf("https://%s", host),
	)

	err = apim.PatchServiceURL(ctx, apim.APIMConfig{
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ServiceName:    serviceName,
		APIID:          ingress.Name,
		RoutePrefix:    routePrefix,
		ServiceURL:     fmt.Sprintf("https://%s", host),
		BearerToken:    token,
	})
	if err != nil {
		logger.Error(err, "🚫 Failed to patch API in APIM")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	logger.Info("✅ Successfully patched API in APIM",
		"apiID", ingress.Name,
		"serviceUrl", fmt.Sprintf("https://%s", host),
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IngressWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Named("ingresswatcher").
		Complete(r)
}
