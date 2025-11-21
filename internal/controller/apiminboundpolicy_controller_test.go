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

var _ = Describe("APIMInboundPolicy Controller", func() {
	const resourceName = "test-apim-inbound-policy"
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

		By("creating the APIMInboundPolicy resource")
		apimPolicy := &apimv1.APIMInboundPolicy{}
		err = k8sClient.Get(ctx, typeNamespacedName, apimPolicy)
		if err != nil && errors.IsNotFound(err) {
			apimPolicy = &apimv1.APIMInboundPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: apimv1.APIMInboundPolicySpec{
					APIMService:   apimServiceName,
					APIID:         "test-api-id",
					PolicyContent: "<policies><inbound><base /></inbound></policies>",
				},
			}
			Expect(k8sClient.Create(ctx, apimPolicy)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up the APIMInboundPolicy resource")
		resource := &apimv1.APIMInboundPolicy{}
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
			controllerReconciler := &APIMInboundPolicyReconciler{
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

		It("should handle missing APIMService gracefully", func() {
			By("creating a policy with a non-existent APIMService")
			invalidPolicyName := types.NamespacedName{
				Name:      "test-policy-invalid-service",
				Namespace: "default",
			}
			invalidPolicy := &apimv1.APIMInboundPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      invalidPolicyName.Name,
					Namespace: invalidPolicyName.Namespace,
				},
				Spec: apimv1.APIMInboundPolicySpec{
					APIMService:   "non-existent-service",
					APIID:         "test-api-id",
					PolicyContent: "<policies><inbound><base /></inbound></policies>",
				},
			}
			Expect(k8sClient.Create(ctx, invalidPolicy)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, invalidPolicy)).To(Succeed())
			}()

			By("reconciling the resource")
			controllerReconciler := &APIMInboundPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: invalidPolicyName,
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
			controllerReconciler := &APIMInboundPolicyReconciler{
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
			policy := &apimv1.APIMInboundPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, policy)).To(Succeed())
			Expect(policy.Status.Phase).To(Equal("Error"))
			Expect(policy.Status.Message).To(ContainSubstring("Failed to get Azure token"))
		})

		It("should handle deleted resource gracefully", func() {
			By("deleting the resource")
			policy := &apimv1.APIMInboundPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, policy)).To(Succeed())
			Expect(k8sClient.Delete(ctx, policy)).To(Succeed())

			By("reconciling the deleted resource")
			controllerReconciler := &APIMInboundPolicyReconciler{
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
