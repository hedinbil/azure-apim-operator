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
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

var _ = Describe("APIMAPIDeployment Controller", func() {
	const resourceName = "test-apim-api-deployment"
	const apimServiceName = "test-apim-service"

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	BeforeEach(func() {
		By("creating the APIMService resource as dependency")
		apimServiceNamespacedName := types.NamespacedName{
			Name:      apimServiceName,
			Namespace: "default",
		}
		apimService := &apimv1.APIMService{}
		err := k8sClient.Get(ctx, apimServiceNamespacedName, apimService)
		if err != nil && errors.IsNotFound(err) {
			apimService = &apimv1.APIMService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      apimServiceNamespacedName.Name,
					Namespace: apimServiceNamespacedName.Namespace,
				},
				Spec: apimv1.APIMServiceSpec{
					Name:          "test-apim-service-instance",
					ResourceGroup: "test-rg",
					Subscription:  "test-subscription-id",
				},
			}
			Expect(k8sClient.Create(ctx, apimService)).To(Succeed())
		}

		By("creating the APIMAPI resource as dependency")
		// The APIMAPI must have the same name as the deployment for the controller to find it
		apimAPI := &apimv1.APIMAPI{}
		err = k8sClient.Get(ctx, typeNamespacedName, apimAPI)
		if err != nil && errors.IsNotFound(err) {
			apimAPI = &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName, // Same name as deployment
					Namespace: "default",
				},
				Spec: apimv1.APIMAPISpec{
					APIID:       "test-api-id",
					APIMService: apimServiceName,
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
					APIMService:          apimServiceName,
					Subscription:         "test-subscription-id",
					ResourceGroup:        "test-rg",
					RoutePrefix:          "/test-api",
					ServiceURL:           "https://example.com/api",
					OpenAPIDefinitionURL: "https://petstore3.swagger.io/api/v3/openapi.json",
					SubscriptionRequired: true,
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up Pod resources")
		pods := &corev1.PodList{}
		_ = k8sClient.List(ctx, pods)
		for i := range pods.Items {
			if pods.Items[i].Namespace == "default" {
				_ = k8sClient.Delete(ctx, &pods.Items[i])
			}
		}

		By("cleaning up ReplicaSet resources")
		replicaSets := &appsv1.ReplicaSetList{}
		_ = k8sClient.List(ctx, replicaSets)
		for i := range replicaSets.Items {
			if replicaSets.Items[i].Namespace == "default" {
				_ = k8sClient.Delete(ctx, &replicaSets.Items[i])
			}
		}

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

		By("cleaning up the APIMService resource")
		apimService := &apimv1.APIMService{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: apimServiceName, Namespace: "default"}, apimService)
		if err == nil {
			Expect(k8sClient.Delete(ctx, apimService)).To(Succeed())
		}
	})

	Context("When reconciling a resource", func() {
		It("should keep deployment and report waiting for a selector match", func() {
			By("reconciling the resource without any matching ReplicaSet")
			controllerReconciler := &APIMAPIDeploymentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying the deployment remains and reports WaitingForMatch")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseWaitingForMatch))
			Expect(deployment.Status.Status).To(Equal(apimDeploymentStatusPending))
			Expect(deployment.Status.Message).To(ContainSubstring("matched 0 ReplicaSets"))
		})

		It("should persist an error status when Azure credentials are missing", func() {
			By("serving a local OpenAPI document")
			server := newOpenAPIServer()
			defer server.Close()

			By("creating a matching ready ReplicaSet")
			rs := createReplicaSet(ctx, "test-deployment-replicaset", map[string]string{"app.kubernetes.io/name": resourceName}, map[string]string{"app": resourceName}, 1)
			createReadyPodForReplicaSet(ctx, rs, "test-deployment-pod")

			By("pointing the deployment at the local OpenAPI document")
			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			deployment.Spec.OpenAPIDefinitionURL = server.URL
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			By("ensuring Azure credentials are not set")
			restoreIdentityEnv := unsetAzureIdentityEnvVars()
			defer restoreIdentityEnv()

			By("reconciling the resource")
			controllerReconciler := &APIMAPIDeploymentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying that the failure is recorded on the deployment")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			updatedDeployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedDeployment)).To(Succeed())
			Expect(updatedDeployment.Status.Phase).To(Equal(phaseError))
			Expect(updatedDeployment.Status.Status).To(Equal(phaseError))
			Expect(updatedDeployment.Status.Message).To(ContainSubstring("AZURE_CLIENT_ID or AZURE_TENANT_ID not set"))
			Expect(updatedDeployment.Status.LastError).To(ContainSubstring("missing AZURE_CLIENT_ID or AZURE_TENANT_ID"))
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

		It("should resolve APIMAPI via explicit apimApiName reference", func() {
			By("serving a local OpenAPI document")
			server := newOpenAPIServer()
			defer server.Close()

			By("creating a matching ready ReplicaSet for the referenced APIMAPI")
			rs := createReplicaSet(ctx, "test-referenced-api-replicaset", map[string]string{"app.kubernetes.io/name": resourceName}, map[string]string{"app": resourceName}, 1)
			createReadyPodForReplicaSet(ctx, rs, "test-referenced-api-pod")

			By("ensuring Azure credentials are not set")
			restoreIdentityEnv := unsetAzureIdentityEnvVars()
			defer restoreIdentityEnv()

			By("creating a deployment that references APIMAPI by spec.apimApiName")
			referencedDeploymentName := types.NamespacedName{
				Name:      "test-deployment-api-ref",
				Namespace: "default",
			}
			referencedDeployment := &apimv1.APIMAPIDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      referencedDeploymentName.Name,
					Namespace: referencedDeploymentName.Namespace,
				},
				Spec: apimv1.APIMAPIDeploymentSpec{
					APIMAPIName:          resourceName,
					APIID:                "test-api-id",
					APIMService:          apimServiceName,
					Subscription:         "test-subscription-id",
					ResourceGroup:        "test-rg",
					RoutePrefix:          "/test-api",
					ServiceURL:           "https://example.com/api",
					OpenAPIDefinitionURL: server.URL,
					SubscriptionRequired: true,
				},
			}
			Expect(k8sClient.Create(ctx, referencedDeployment)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, referencedDeployment)
			}()

			By("reconciling the resource")
			controllerReconciler := &APIMAPIDeploymentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: referencedDeploymentName,
			})

			By("verifying that the explicit APIMAPI reference was used")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			updatedDeployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, referencedDeploymentName, updatedDeployment)).To(Succeed())
			Expect(updatedDeployment.Status.Phase).To(Equal(phaseError))
			Expect(updatedDeployment.Status.LastError).To(ContainSubstring("missing AZURE_CLIENT_ID or AZURE_TENANT_ID"))
		})

		It("should skip APIM import when the desired hash is already applied", func() {
			By("serving a local OpenAPI document")
			server := newOpenAPIServer()
			defer server.Close()

			By("creating a matching ready ReplicaSet")
			rs := createReplicaSet(ctx, "test-hash-replicaset", map[string]string{"app.kubernetes.io/name": resourceName}, map[string]string{"app": resourceName}, 1)
			createReadyPodForReplicaSet(ctx, rs, "test-hash-pod")

			By("configuring the deployment to point at the local OpenAPI document")
			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			deployment.Spec.OpenAPIDefinitionURL = server.URL
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			By("precomputing and storing the applied hash")
			openAPIContent := []byte(`{"openapi":"3.0.0","info":{"title":"test","version":"1.0.0"},"paths":{}}`)
			openAPIHash := sha256Hex(openAPIContent)
			desiredHash, err := buildDesiredAPIMStateHash(&deployment.Spec, deployment.Spec.Subscription, deployment.Spec.ResourceGroup, openAPIHash)
			Expect(err).NotTo(HaveOccurred())
			deployment.Status.AppliedHash = desiredHash
			deployment.Status.DesiredHash = desiredHash
			deployment.Status.Phase = apimDeploymentPhaseSucceeded
			deployment.Status.Status = "OK"
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			By("ensuring Azure credentials are not set")
			restoreIdentityEnv := unsetAzureIdentityEnvVars()
			defer restoreIdentityEnv()

			By("reconciling the resource")
			controllerReconciler := &APIMAPIDeploymentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			By("verifying that APIM import is skipped")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			updatedDeployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedDeployment)).To(Succeed())
			Expect(updatedDeployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(updatedDeployment.Status.Status).To(Equal("OK"))
			Expect(updatedDeployment.Status.Message).To(ContainSubstring("No changes detected"))
			Expect(updatedDeployment.Status.AppliedHash).To(Equal(desiredHash))
		})
	})
})

func newOpenAPIServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openapi":"3.0.0","info":{"title":"test","version":"1.0.0"},"paths":{}}`))
	}))
}

func unsetAzureIdentityEnvVars() func() {
	originalClientID := os.Getenv("AZURE_CLIENT_ID")
	originalTenantID := os.Getenv("AZURE_TENANT_ID")
	_ = os.Unsetenv("AZURE_CLIENT_ID")
	_ = os.Unsetenv("AZURE_TENANT_ID")

	return func() {
		if originalClientID != "" {
			_ = os.Setenv("AZURE_CLIENT_ID", originalClientID)
		} else {
			_ = os.Unsetenv("AZURE_CLIENT_ID")
		}
		if originalTenantID != "" {
			_ = os.Setenv("AZURE_TENANT_ID", originalTenantID)
		} else {
			_ = os.Unsetenv("AZURE_TENANT_ID")
		}
	}
}
