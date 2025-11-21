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
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

var _ = Describe("APIMAPIDeployment Controller", func() {
	const resourceName = "test-apim-api-deployment"

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	BeforeEach(func() {
		By("creating the APIMAPI resource as dependency")
		// The APIMAPI must have the same name as the deployment for the controller to find it
		apimAPI := &apimv1.APIMAPI{}
		err := k8sClient.Get(ctx, typeNamespacedName, apimAPI)
		if err != nil && errors.IsNotFound(err) {
			apimAPI = &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName, // Same name as deployment
					Namespace: "default",
				},
				Spec: apimv1.APIMAPISpec{
					APIID:       "test-api-id",
					APIMService: "test-apim-service",
				},
			}
			Expect(k8sClient.Create(ctx, apimAPI)).To(Succeed())
		}

		By("creating the APIMAPIDeployment resource")
		deployment := &apimv1.APIMAPIDeployment{}
		err = k8sClient.Get(ctx, typeNamespacedName, deployment)
		if err != nil && errors.IsNotFound(err) {
			deployment = &apimv1.APIMAPIDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: apimv1.APIMAPIDeploymentSpec{
					APIID:                "test-api-id",
					APIMService:          "test-apim-service",
					Subscription:         "test-subscription-id",
					ResourceGroup:        "test-rg",
					RoutePrefix:          "/test-api",
					ServiceURL:           "https://example.com/api",
					OpenAPIDefinitionURL: "https://example.com/openapi.json",
					SubscriptionRequired: true,
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up the APIMAPIDeployment resource")
		resource := &apimv1.APIMAPIDeployment{}
		err := k8sClient.Get(ctx, typeNamespacedName, resource)
		if err == nil {
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		}

		By("cleaning up the APIMAPI resource")
		apimAPI := &apimv1.APIMAPI{}
		err = k8sClient.Get(ctx, typeNamespacedName, apimAPI) // Same name as deployment
		if err == nil {
			Expect(k8sClient.Delete(ctx, apimAPI)).To(Succeed())
		}
	})

	Context("When reconciling a resource", func() {
		It("should handle missing Azure credentials gracefully", func() {
			By("ensuring Azure credentials are not set")
			originalClientID := os.Getenv("AZURE_CLIENT_ID")
			originalTenantID := os.Getenv("AZURE_TENANT_ID")
			defer func() {
				if originalClientID != "" {
					os.Setenv("AZURE_CLIENT_ID", originalClientID)
				} else {
					os.Unsetenv("AZURE_CLIENT_ID")
				}
				if originalTenantID != "" {
					os.Setenv("AZURE_TENANT_ID", originalTenantID)
				} else {
					os.Unsetenv("AZURE_TENANT_ID")
				}
			}()
			os.Unsetenv("AZURE_CLIENT_ID")
			os.Unsetenv("AZURE_TENANT_ID")

			By("reconciling the resource")
			controllerReconciler := &APIMAPIDeploymentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying that an error is returned")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing AZURE_CLIENT_ID or AZURE_TENANT_ID"))
			Expect(result.Requeue).To(BeFalse())
		})

		It("should handle missing APIMAPI dependency gracefully", func() {
			By("creating a deployment with non-existent APIMAPI")
			invalidDeploymentName := types.NamespacedName{
				Name:      "test-deployment-invalid-api",
				Namespace: "default",
			}
			invalidDeployment := &apimv1.APIMAPIDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      invalidDeploymentName.Name,
					Namespace: invalidDeploymentName.Namespace,
				},
				Spec: apimv1.APIMAPIDeploymentSpec{
					APIID:                "test-api-id",
					APIMService:          "test-apim-service",
					Subscription:         "test-subscription-id",
					ResourceGroup:        "test-rg",
					OpenAPIDefinitionURL: "https://example.com/openapi.json",
				},
			}
			Expect(k8sClient.Create(ctx, invalidDeployment)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, invalidDeployment)).To(Succeed())
			}()

			By("reconciling the resource")
			controllerReconciler := &APIMAPIDeploymentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: invalidDeploymentName,
			})

			By("verifying that the error is handled gracefully")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should handle deleted resource gracefully", func() {
			By("deleting the resource")
			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())

			By("reconciling the deleted resource")
			controllerReconciler := &APIMAPIDeploymentReconciler{
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
	})
})
