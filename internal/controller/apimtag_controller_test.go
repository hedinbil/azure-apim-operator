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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

var _ = Describe("APIMTag Controller", func() {
	const resourceName = "test-apim-tag"
	const apimServiceName = "test-apim-service"

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}
	apimServiceNamespacedName := types.NamespacedName{
		Name:      apimServiceName,
		Namespace: "default",
	}

	BeforeEach(func() {
		By("creating the APIMService resource")
		apimService := &apimv1.APIMService{}
		err := k8sClient.Get(ctx, apimServiceNamespacedName, apimService)
		if err != nil && errors.IsNotFound(err) {
			apimService = &apimv1.APIMService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      apimServiceName,
					Namespace: "default",
				},
				Spec: apimv1.APIMServiceSpec{
					Name:          "test-apim",
					ResourceGroup: "test-rg",
					Subscription:  "test-subscription-id",
				},
			}
			Expect(k8sClient.Create(ctx, apimService)).To(Succeed())
		}

		By("creating the APIMTag resource")
		apimTag := &apimv1.APIMTag{}
		err = k8sClient.Get(ctx, typeNamespacedName, apimTag)
		if err != nil && errors.IsNotFound(err) {
			apimTag = &apimv1.APIMTag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: apimv1.APIMTagSpec{
					APIMService: apimServiceName,
					TagID:       "test-tag-id",
					DisplayName: "Test Tag",
				},
			}
			Expect(k8sClient.Create(ctx, apimTag)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up the APIMTag resource")
		resource := &apimv1.APIMTag{}
		err := k8sClient.Get(ctx, typeNamespacedName, resource)
		if err == nil {
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		}

		By("cleaning up the APIMService resource")
		apimService := &apimv1.APIMService{}
		err = k8sClient.Get(ctx, apimServiceNamespacedName, apimService)
		if err == nil {
			Expect(k8sClient.Delete(ctx, apimService)).To(Succeed())
		}
	})

	Context("When reconciling a resource", func() {
		It("should handle missing Azure credentials gracefully", func() {
			By("ensuring Azure credentials are not set")
			// Save original values if they exist
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
			controllerReconciler := &APIMTagReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying that no error is returned and status is updated")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that the status is set to Error")
			var tag apimv1.APIMTag
			Expect(k8sClient.Get(ctx, typeNamespacedName, &tag)).To(Succeed())
			Expect(tag.Status.Phase).To(Equal("Error"))
			Expect(tag.Status.Message).To(ContainSubstring("missing AZURE_CLIENT_ID or AZURE_TENANT_ID"))
		})

		It("should handle missing APIMService gracefully", func() {
			By("creating a tag with a non-existent APIMService")
			invalidTagName := types.NamespacedName{
				Name:      "test-tag-invalid-service",
				Namespace: "default",
			}
			invalidTag := &apimv1.APIMTag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      invalidTagName.Name,
					Namespace: invalidTagName.Namespace,
				},
				Spec: apimv1.APIMTagSpec{
					APIMService: "non-existent-service",
					TagID:       "test-tag-id",
					DisplayName: "Test Tag",
				},
			}
			Expect(k8sClient.Create(ctx, invalidTag)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, invalidTag)).To(Succeed())
			}()

			By("reconciling the resource")
			controllerReconciler := &APIMTagReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: invalidTagName,
			})

			By("verifying that the error is handled gracefully")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should update status when Azure token retrieval fails", func() {
			By("setting invalid Azure credentials")
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
			os.Setenv("AZURE_CLIENT_ID", "invalid-client-id")
			os.Setenv("AZURE_TENANT_ID", "invalid-tenant-id")

			By("reconciling the resource")
			controllerReconciler := &APIMTagReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying that reconciliation is requeued")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			By("verifying that status is updated with error")
			tag := &apimv1.APIMTag{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tag)).To(Succeed())
			Expect(tag.Status.Phase).To(Equal("Error"))
			Expect(tag.Status.Message).To(ContainSubstring("Failed to get Azure token"))
		})

		It("should handle deleted resource gracefully", func() {
			By("deleting the resource")
			tag := &apimv1.APIMTag{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tag)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tag)).To(Succeed())

			By("reconciling the deleted resource")
			controllerReconciler := &APIMTagReconciler{
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
