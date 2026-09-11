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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

var _ = Describe("APIMProduct Controller", func() {
	const resourceName = "test-apim-product"
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
					Subscription:  "00000000-0000-0000-0000-000000000001",
				},
			}
			Expect(k8sClient.Create(ctx, apimService)).To(Succeed())
		}

		By("creating the APIMProduct resource")
		apimProduct := &apimv1.APIMProduct{}
		err = k8sClient.Get(ctx, typeNamespacedName, apimProduct)
		if err != nil && errors.IsNotFound(err) {
			apimProduct = &apimv1.APIMProduct{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: apimv1.APIMProductSpec{
					APIMService: apimServiceName,
					ProductID:   "test-product-id",
					DisplayName: "Test Product",
					Description: "Test Product Description",
					Published:   false,
				},
			}
			Expect(k8sClient.Create(ctx, apimProduct)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up the APIMProduct resource")
		forceDeleteProduct(ctx, typeNamespacedName)

		By("cleaning up the APIMService resource")
		apimService := &apimv1.APIMService{}
		err := k8sClient.Get(ctx, apimServiceNamespacedName, apimService)
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
					Expect(os.Setenv("AZURE_CLIENT_ID", originalClientID)).To(Succeed())
				} else {
					Expect(os.Unsetenv("AZURE_CLIENT_ID")).To(Succeed())
				}
				if originalTenantID != "" {
					Expect(os.Setenv("AZURE_TENANT_ID", originalTenantID)).To(Succeed())
				} else {
					Expect(os.Unsetenv("AZURE_TENANT_ID")).To(Succeed())
				}
			}()
			Expect(os.Unsetenv("AZURE_CLIENT_ID")).To(Succeed())
			Expect(os.Unsetenv("AZURE_TENANT_ID")).To(Succeed())
			By("reconciling the resource")
			controllerReconciler := &APIMProductReconciler{
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
			var product apimv1.APIMProduct
			Expect(k8sClient.Get(ctx, typeNamespacedName, &product)).To(Succeed())
			Expect(product.Status.Phase).To(Equal("Error"))
			Expect(product.Status.Message).To(ContainSubstring("missing AZURE_CLIENT_ID or AZURE_TENANT_ID"))
		})

		It("should handle missing APIMService gracefully", func() {
			By("creating a product with a non-existent APIMService")
			invalidProductName := types.NamespacedName{
				Name:      "test-product-invalid-service",
				Namespace: "default",
			}
			invalidProduct := &apimv1.APIMProduct{
				ObjectMeta: metav1.ObjectMeta{
					Name:      invalidProductName.Name,
					Namespace: invalidProductName.Namespace,
				},
				Spec: apimv1.APIMProductSpec{
					APIMService: "non-existent-service",
					ProductID:   "test-product-id",
					DisplayName: "Test Product",
				},
			}
			Expect(k8sClient.Create(ctx, invalidProduct)).To(Succeed())
			defer forceDeleteProduct(ctx, invalidProductName)

			By("reconciling the resource")
			controllerReconciler := &APIMProductReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: invalidProductName,
			})

			By("verifying that the missing dependency is reported and retried")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueMissingAPIMService))
			Expect(k8sClient.Get(ctx, invalidProductName, invalidProduct)).To(Succeed())
			Expect(invalidProduct.Status.Phase).To(Equal(phaseError))
			Expect(invalidProduct.Status.Message).To(Equal(`APIMService "non-existent-service" not found in namespace default`))
			Expect(controllerutil.ContainsFinalizer(invalidProduct, productFinalizer)).To(BeTrue())
		})

		It("should add the finalizer and create the product in APIM", func() {
			restore := stubAzureIdentityEnv()
			defer restore()

			var upserted []apim.APIMProductConfig
			controllerReconciler := &APIMProductReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				getToken: func(context.Context, string, string) (string, error) { return "token", nil },
				upsertProduct: func(_ context.Context, cfg apim.APIMProductConfig) error {
					upserted = append(upserted, cfg)
					return nil
				},
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())
			Expect(upserted).To(HaveLen(1))
			Expect(upserted[0].ProductID).To(Equal("test-product-id"))
			Expect(upserted[0].ServiceName).To(Equal(apimServiceName))
			Expect(upserted[0].SubscriptionID).To(Equal("00000000-0000-0000-0000-000000000001"))
			Expect(upserted[0].ResourceGroup).To(Equal("test-rg"))
			Expect(upserted[0].BearerToken).To(Equal("token"))

			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
			Expect(product.Status.Phase).To(Equal(phaseCreated))
			Expect(product.Spec.DeletionPolicy).To(Equal(apimv1.DeletionPolicyDelete), "the API server should default deletionPolicy")
		})

		It("should delete the product in APIM before letting the resource go", func() {
			restore := stubAzureIdentityEnv()
			defer restore()

			var deleted []apim.APIMProductConfig
			controllerReconciler := &APIMProductReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				getToken:      func(context.Context, string, string) (string, error) { return "token", nil },
				upsertProduct: func(context.Context, apim.APIMProductConfig) error { return nil },
				deleteProduct: func(_ context.Context, cfg apim.APIMProductConfig) error {
					deleted = append(deleted, cfg)
					return nil
				},
			}
			req := reconcile.Request{NamespacedName: typeNamespacedName}

			By("reconciling once so the finalizer is in place")
			_, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("deleting the resource")
			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(k8sClient.Delete(ctx, product)).To(Succeed())
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed(), "the finalizer keeps the resource until APIM is cleaned up")
			Expect(product.DeletionTimestamp.IsZero()).To(BeFalse())

			By("reconciling the deletion")
			result, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())
			Expect(deleted).To(HaveLen(1))
			Expect(deleted[0].ProductID).To(Equal("test-product-id"))
			Expect(deleted[0].ServiceName).To(Equal(apimServiceName))
			Expect(deleted[0].BearerToken).To(Equal("token"))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("should keep the resource when the product cannot be deleted in APIM", func() {
			restore := stubAzureIdentityEnv()
			defer restore()

			controllerReconciler := &APIMProductReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				getToken:      func(context.Context, string, string) (string, error) { return "token", nil },
				upsertProduct: func(context.Context, apim.APIMProductConfig) error { return nil },
				deleteProduct: func(context.Context, apim.APIMProductConfig) error {
					return fmt.Errorf("failed to delete product: 400 Bad Request: product has active subscriptions")
				},
			}
			req := reconcile.Request{NamespacedName: typeNamespacedName}
			_, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(k8sClient.Delete(ctx, product)).To(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed(), "the resource must stay until APIM is cleaned up")
			Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
			Expect(product.Status.Phase).To(Equal(phaseError))
			Expect(product.Status.Message).To(ContainSubstring("active subscriptions"))
		})

		It("should keep the product in APIM when deletionPolicy is Retain", func() {
			restore := stubAzureIdentityEnv()
			defer restore()

			controllerReconciler := &APIMProductReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				getToken:      func(context.Context, string, string) (string, error) { return "token", nil },
				upsertProduct: func(context.Context, apim.APIMProductConfig) error { return nil },
				deleteProduct: func(context.Context, apim.APIMProductConfig) error {
					Fail("deleteProduct must not be called with deletionPolicy Retain")
					return nil
				},
			}
			req := reconcile.Request{NamespacedName: typeNamespacedName}

			By("marking the product as retained")
			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			product.Spec.DeletionPolicy = apimv1.DeletionPolicyRetain
			Expect(k8sClient.Update(ctx, product)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("deleting the resource and reconciling")
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(k8sClient.Delete(ctx, product)).To(Succeed())
			result, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("should release the finalizer on deletion when no Azure identity is configured", func() {
			restore := unsetAzureIdentityEnvVars()
			defer restore()

			controllerReconciler := &APIMProductReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				deleteProduct: func(context.Context, apim.APIMProductConfig) error {
					Fail("deleteProduct must not be called without an identity")
					return nil
				},
			}
			req := reconcile.Request{NamespacedName: typeNamespacedName}

			By("reconciling once: the finalizer is taken and the missing identity is reported")
			result, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
			Expect(product.Status.Message).To(Equal(errMsgMissingAzureIdentity))

			By("deleting the resource: the delete must not wedge on the finalizer")
			Expect(k8sClient.Delete(ctx, product)).To(Succeed())
			result, err = controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("should release the finalizer when the APIMService is gone", func() {
			restore := stubAzureIdentityEnv()
			defer restore()

			controllerReconciler := &APIMProductReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				getToken:      func(context.Context, string, string) (string, error) { return "token", nil },
				upsertProduct: func(context.Context, apim.APIMProductConfig) error { return nil },
				deleteProduct: func(context.Context, apim.APIMProductConfig) error {
					Fail("deleteProduct must not be called without an APIMService")
					return nil
				},
			}
			req := reconcile.Request{NamespacedName: typeNamespacedName}
			_, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("removing the APIMService and then the product")
			apimService := &apimv1.APIMService{}
			Expect(k8sClient.Get(ctx, apimServiceNamespacedName, apimService)).To(Succeed())
			Expect(k8sClient.Delete(ctx, apimService)).To(Succeed())
			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(k8sClient.Delete(ctx, product)).To(Succeed())

			result, err := controllerReconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("should update status when Azure token retrieval fails", func() {
			By("setting invalid Azure credentials")
			originalClientID := os.Getenv("AZURE_CLIENT_ID")
			originalTenantID := os.Getenv("AZURE_TENANT_ID")
			defer func() {
				if originalClientID != "" {
					Expect(os.Setenv("AZURE_CLIENT_ID", originalClientID)).To(Succeed())
				} else {
					Expect(os.Unsetenv("AZURE_CLIENT_ID")).To(Succeed())
				}
				if originalTenantID != "" {
					Expect(os.Setenv("AZURE_TENANT_ID", originalTenantID)).To(Succeed())
				} else {
					Expect(os.Unsetenv("AZURE_TENANT_ID")).To(Succeed())
				}
			}()
			Expect(os.Setenv("AZURE_CLIENT_ID", "invalid-client-id")).To(Succeed())
			Expect(os.Setenv("AZURE_TENANT_ID", "invalid-tenant-id")).To(Succeed())
			By("reconciling the resource")
			controllerReconciler := &APIMProductReconciler{
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
			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(product.Status.Phase).To(Equal("Error"))
			Expect(product.Status.Message).To(ContainSubstring("Failed to get Azure token"))
		})

		It("should handle deleted resource gracefully", func() {
			By("deleting the resource")
			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, product)).To(Succeed())
			Expect(k8sClient.Delete(ctx, product)).To(Succeed())

			By("reconciling the deleted resource")
			controllerReconciler := &APIMProductReconciler{
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
	})
})

// stubAzureIdentityEnv sets placeholder identity variables and returns a restore func.
func stubAzureIdentityEnv() func() {
	restore := unsetAzureIdentityEnvVars()
	_ = os.Setenv("AZURE_CLIENT_ID", "test-client-id")
	_ = os.Setenv("AZURE_TENANT_ID", "test-tenant-id")
	return restore
}

// forceDeleteProduct deletes a product and strips the finalizer, which no controller
// would otherwise release in envtest.
func forceDeleteProduct(ctx context.Context, key types.NamespacedName) {
	product := &apimv1.APIMProduct{}
	if err := k8sClient.Get(ctx, key, product); err != nil {
		return
	}
	_ = k8sClient.Delete(ctx, product)
	if err := k8sClient.Get(ctx, key, product); err != nil {
		return
	}
	if controllerutil.RemoveFinalizer(product, productFinalizer) {
		Expect(client.IgnoreNotFound(k8sClient.Update(ctx, product))).To(Succeed())
	}
	Eventually(func() bool {
		return errors.IsNotFound(k8sClient.Get(ctx, key, &apimv1.APIMProduct{}))
	}).Should(BeTrue())
}
