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

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	BeforeEach(func() {
		By("creating the APIMAPI resource")
		apimAPI := &apimv1.APIMAPI{}
		err := k8sClient.Get(ctx, typeNamespacedName, apimAPI)
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
				Status: apimv1.APIMAPIStatus{
					ApiHost:             "https://test-apim.azure-api.net/test-api",
					DeveloperPortalHost: "https://test-apim.developer.azure-api.net",
					Status:              "OK",
				},
			}
			Expect(k8sClient.Create(ctx, apimAPI)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up the APIMAPI resource")
		resource := &apimv1.APIMAPI{}
		err := k8sClient.Get(ctx, typeNamespacedName, resource)
		if err == nil {
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
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
			Expect(result.Requeue).To(BeFalse())

			By("verifying ArgoCD annotation is set")
			api := &apimv1.APIMAPI{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, api)).To(Succeed())
			Expect(api.Annotations).NotTo(BeNil())
			Expect(api.Annotations["link.argocd.argoproj.io/external-link"]).To(Equal("https://test-apim.azure-api.net/test-api"))
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
					APIID:       "test-api-id-2",
					APIMService: "test-apim-service",
				},
				Status: apimv1.APIMAPIStatus{
					ApiHost: "https://test-apim.azure-api.net/test-api-2",
				},
			}
			Expect(k8sClient.Create(ctx, api)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, api)).To(Succeed())
			}()

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
			Expect(result.Requeue).To(BeFalse())

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
			Expect(result.Requeue).To(BeFalse())
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
	})
})
