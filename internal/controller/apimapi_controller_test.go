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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

var _ = Describe("APIMAPI Controller", func() {
	const resourceName = "test-apim-api"
	const apimServiceName = "test-apim-service"

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	BeforeEach(func() {
		By("creating the APIMService dependency")
		serviceNamespacedName := types.NamespacedName{
			Name:      apimServiceName,
			Namespace: "default",
		}
		apimService := &apimv1.APIMService{}
		err := k8sClient.Get(ctx, serviceNamespacedName, apimService)
		if err != nil && errors.IsNotFound(err) {
			apimService = &apimv1.APIMService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceNamespacedName.Name,
					Namespace: serviceNamespacedName.Namespace,
				},
				Spec: apimv1.APIMServiceSpec{
					Name:          "test-apim-service-instance",
					ResourceGroup: "test-rg",
					Subscription:  "00000000-0000-0000-0000-000000000001",
				},
			}
			Expect(k8sClient.Create(ctx, apimService)).To(Succeed())
		}

		By("creating the APIMAPI resource")
		apimAPI := &apimv1.APIMAPI{}
		err = k8sClient.Get(ctx, typeNamespacedName, apimAPI)
		if err != nil && errors.IsNotFound(err) {
			apimAPI = &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: apimv1.APIMAPISpec{
					APIID:                "test-api-id",
					APIMService:          "test-apim-service",
					RoutePrefix:          "/test-api",
					ServiceURL:           "https://example.com/api",
					OpenAPIDefinitionURL: "https://example.com/openapi.json",
					SubscriptionRequired: true,
				},
			}
			Expect(k8sClient.Create(ctx, apimAPI)).To(Succeed())

			// Update status separately since status is not persisted on create
			apimAPI.Status = apimv1.APIMAPIStatus{
				ApiHost:             "https://test-apim.azure-api.net/test-api",
				DeveloperPortalHost: "https://test-apim.developer.azure-api.net",
				Status:              "OK",
			}
			Expect(k8sClient.Status().Update(ctx, apimAPI)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up the APIMAPIDeployment resource")
		deployment := &apimv1.APIMAPIDeployment{}
		err := k8sClient.Get(ctx, typeNamespacedName, deployment)
		if err == nil {
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		}

		By("cleaning up the APIMAPI resource")
		resource := &apimv1.APIMAPI{}
		err = k8sClient.Get(ctx, typeNamespacedName, resource)
		if err == nil {
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		}

		By("cleaning up the APIMService resource")
		apimService := &apimv1.APIMService{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: apimServiceName, Namespace: "default"}, apimService)
		if err == nil {
			Expect(k8sClient.Delete(ctx, apimService)).To(Succeed())
		}
	})

	Context("When reconciling a resource", func() {
		It("should update ArgoCD external link annotation when status has ApiHost", func() {
			By("reconciling the resource")
			controllerReconciler := &APIMAPIReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying reconciliation succeeds")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())

			By("verifying ArgoCD annotation is set")
			api := &apimv1.APIMAPI{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, api)).To(Succeed())
			Expect(api.Annotations).NotTo(BeNil())
			Expect(api.Annotations["link.argocd.argoproj.io/external-link"]).To(Equal("https://test-apim.azure-api.net/test-api"))

			By("verifying that APIMAPIDeployment is ensured")
			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Spec.APIMAPIName).To(Equal(resourceName))
			Expect(deployment.Spec.APIID).To(Equal("test-api-id"))
		})

		It("should initialize annotations map if nil", func() {
			By("creating an APIMAPI with nil annotations")
			apiName := types.NamespacedName{
				Name:      "test-api-nil-annotations",
				Namespace: "default",
			}
			api := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:        apiName.Name,
					Namespace:   apiName.Namespace,
					Annotations: nil, // Explicitly nil
				},
				Spec: apimv1.APIMAPISpec{
					ServiceURL:           "https://example.com/api",
					RoutePrefix:          "/test-api",
					OpenAPIDefinitionURL: "https://example.com/openapi.json",
					APIID:                "test-api-id-2",
					APIMService:          "test-apim-service",
				},
			}
			Expect(k8sClient.Create(ctx, api)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, api)).To(Succeed())
			}()

			// Update status separately since status is not persisted on create
			api.Status = apimv1.APIMAPIStatus{
				ApiHost: "https://test-apim.azure-api.net/test-api-2",
			}
			Expect(k8sClient.Status().Update(ctx, api)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &APIMAPIReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: apiName,
			})

			By("verifying reconciliation succeeds")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())

			By("verifying annotations map is initialized")
			updatedAPI := &apimv1.APIMAPI{}
			Expect(k8sClient.Get(ctx, apiName, updatedAPI)).To(Succeed())
			Expect(updatedAPI.Annotations).NotTo(BeNil())
			Expect(updatedAPI.Annotations["link.argocd.argoproj.io/external-link"]).To(Equal("https://test-apim.azure-api.net/test-api-2"))
		})

		It("should handle deleted resource gracefully", func() {
			By("deleting the resource")
			api := &apimv1.APIMAPI{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, api)).To(Succeed())
			Expect(k8sClient.Delete(ctx, api)).To(Succeed())

			By("reconciling the deleted resource")
			controllerReconciler := &APIMAPIReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying that deletion is handled gracefully")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())
		})

		It("should update annotation when ApiHost changes", func() {
			By("reconciling initially")
			controllerReconciler := &APIMAPIReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("updating the ApiHost in status")
			api := &apimv1.APIMAPI{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, api)).To(Succeed())
			api.Status.ApiHost = "https://new-host.azure-api.net/test-api"
			Expect(k8sClient.Status().Update(ctx, api)).To(Succeed())

			By("reconciling again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying annotation is updated")
			updatedAPI := &apimv1.APIMAPI{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedAPI)).To(Succeed())
			Expect(updatedAPI.Annotations["link.argocd.argoproj.io/external-link"]).To(Equal("https://new-host.azure-api.net/test-api"))
		})

		It("should carry a websocket API's type, display name and protocols onto its deployment", func() {
			By("creating a websocket APIMAPI without an OpenAPI URL")
			wsName := types.NamespacedName{Name: "test-apim-api-websocket", Namespace: "default"}
			wsAPI := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{Name: wsName.Name, Namespace: wsName.Namespace},
				Spec: apimv1.APIMAPISpec{
					Type: apimv1.APITypeWebSocket,
					WebSocket: &apimv1.APIMAPIWebSocket{
						DisplayName: "Orders hub",
						Protocols:   []string{"ws", "wss"},
					},
					APIID:       "orders-hub",
					APIMService: apimServiceName,
					RoutePrefix: "/orders/hub",
					ServiceURL:  "wss://orders.example.com/hub",
				},
			}
			Expect(k8sClient.Create(ctx, wsAPI)).To(Succeed())
			defer func() {
				deployment := &apimv1.APIMAPIDeployment{}
				if err := k8sClient.Get(ctx, wsName, deployment); err == nil {
					_ = k8sClient.Delete(ctx, deployment)
				}
				_ = k8sClient.Delete(ctx, wsAPI)
			}()

			By("reconciling it")
			controllerReconciler := &APIMAPIReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the deployment spec")
			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, wsName, deployment)).To(Succeed())
			Expect(deployment.Spec.Type).To(Equal(apimv1.APITypeWebSocket))
			Expect(deployment.Spec.WebSocket).NotTo(BeNil())
			Expect(deployment.Spec.WebSocket.DisplayName).To(Equal("Orders hub"))
			Expect(deployment.Spec.WebSocket.Protocols).To(Equal([]string{"ws", "wss"}))
			Expect(deployment.Spec.ServiceURL).To(Equal("wss://orders.example.com/hub"))
			Expect(deployment.Spec.OpenAPIDefinitionURL).To(BeEmpty())
		})

		It("should default the type to http and reject specs that mix the two kinds", func() {
			By("defaulting type when it is omitted")
			plain := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{Name: "test-apim-api-default-type", Namespace: "default"},
				Spec: apimv1.APIMAPISpec{
					APIID:                "default-type",
					APIMService:          apimServiceName,
					RoutePrefix:          "/default-type",
					ServiceURL:           "https://example.com/api",
					OpenAPIDefinitionURL: "https://example.com/openapi.json",
				},
			}
			Expect(k8sClient.Create(ctx, plain)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, plain) }()
			Expect(plain.Spec.Type).To(Equal(apimv1.APITypeHTTP))

			By("rejecting an http API without an OpenAPI URL")
			noOpenAPI := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{Name: "test-apim-api-no-openapi", Namespace: "default"},
				Spec: apimv1.APIMAPISpec{
					APIID:       "no-openapi",
					APIMService: apimServiceName,
					RoutePrefix: "/no-openapi",
					ServiceURL:  "https://example.com/api",
				},
			}
			err := k8sClient.Create(ctx, noOpenAPI)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("openApiDefinitionUrl is required unless type is websocket"))

			By("rejecting a websocket API with an https backend")
			wrongScheme := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{Name: "test-apim-api-ws-https", Namespace: "default"},
				Spec: apimv1.APIMAPISpec{
					Type:        apimv1.APITypeWebSocket,
					APIID:       "ws-https",
					APIMService: apimServiceName,
					RoutePrefix: "/ws-https",
					ServiceURL:  "https://example.com/hub",
				},
			}
			err = k8sClient.Create(ctx, wrongScheme)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ws:// or wss:// serviceUrl"))

			By("rejecting an http API with a wss backend")
			httpWss := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{Name: "test-apim-api-http-wss", Namespace: "default"},
				Spec: apimv1.APIMAPISpec{
					APIID:                "http-wss",
					APIMService:          apimServiceName,
					RoutePrefix:          "/http-wss",
					ServiceURL:           "wss://example.com/hub",
					OpenAPIDefinitionURL: "https://example.com/openapi.json",
				},
			}
			err = k8sClient.Create(ctx, httpWss)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("http:// or https:// serviceUrl"))

			By("rejecting an http API that sets the websocket block")
			httpWithBlock := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{Name: "test-apim-api-http-ws-block", Namespace: "default"},
				Spec: apimv1.APIMAPISpec{
					WebSocket:            &apimv1.APIMAPIWebSocket{DisplayName: "Ignored"},
					APIID:                "http-ws-block",
					APIMService:          apimServiceName,
					RoutePrefix:          "/http-ws-block",
					ServiceURL:           "https://example.com/api",
					OpenAPIDefinitionURL: "https://example.com/openapi.json",
				},
			}
			err = k8sClient.Create(ctx, httpWithBlock)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("websocket block is only allowed when type is websocket"))
		})

		It("should create APIMAPIDeployment when reconciling a new APIMAPI", func() {
			By("creating a fresh APIMAPI")
			freshAPIName := types.NamespacedName{Name: "test-apim-api-fresh", Namespace: "default"}
			freshAPI := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:      freshAPIName.Name,
					Namespace: freshAPIName.Namespace,
				},
				Spec: apimv1.APIMAPISpec{
					APIID:                "fresh-api-id",
					APIMService:          apimServiceName,
					RoutePrefix:          "/fresh-api",
					ServiceURL:           "https://example.com/fresh-api",
					OpenAPIDefinitionURL: "https://example.com/fresh-openapi.json",
					SubscriptionRequired: true,
				},
			}
			Expect(k8sClient.Create(ctx, freshAPI)).To(Succeed())
			defer func() {
				deployment := &apimv1.APIMAPIDeployment{}
				if err := k8sClient.Get(ctx, freshAPIName, deployment); err == nil {
					_ = k8sClient.Delete(ctx, deployment)
				}
				_ = k8sClient.Delete(ctx, freshAPI)
			}()

			By("reconciling the fresh APIMAPI")
			controllerReconciler := &APIMAPIReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: freshAPIName})

			By("verifying that the deployment was created")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())

			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, freshAPIName, deployment)).To(Succeed())
			Expect(deployment.Spec.APIMAPIName).To(Equal(freshAPIName.Name))
			Expect(deployment.Spec.APIID).To(Equal("fresh-api-id"))
			Expect(deployment.Spec.RoutePrefix).To(Equal("/fresh-api"))
		})
	})
})
