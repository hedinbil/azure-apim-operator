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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

var _ = Describe("ReplicaSetWatcher Controller", func() {
	const appName = "test-app"
	const apimServiceName = "test-apim-service"

	ctx := context.Background()

	replicaSetNamespacedName := types.NamespacedName{
		Name:      "test-replicaset",
		Namespace: "default",
	}
	apimAPINamespacedName := types.NamespacedName{
		Name:      appName,
		Namespace: "default",
	}
	apimServiceNamespacedName := types.NamespacedName{
		Name:      apimServiceName,
		Namespace: "default",
	}

	BeforeEach(func() {
		By("creating the APIMService resource as dependency")
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

		By("creating the APIMAPI resource as dependency")
		apimAPI := &apimv1.APIMAPI{}
		err = k8sClient.Get(ctx, apimAPINamespacedName, apimAPI)
		if err != nil && errors.IsNotFound(err) {
			apimAPI = &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:      appName,
					Namespace: "default",
				},
				Spec: apimv1.APIMAPISpec{
					APIID:       "test-api-id",
					APIMService: apimServiceName,
				},
			}
			Expect(k8sClient.Create(ctx, apimAPI)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up ReplicaSet resources")
		replicaSets := &appsv1.ReplicaSetList{}
		_ = k8sClient.List(ctx, replicaSets)
		for i := range replicaSets.Items {
			if replicaSets.Items[i].Namespace == "default" {
				_ = k8sClient.Delete(ctx, &replicaSets.Items[i])
			}
		}

		By("cleaning up Pod resources")
		pods := &corev1.PodList{}
		_ = k8sClient.List(ctx, pods)
		for i := range pods.Items {
			if pods.Items[i].Namespace == "default" {
				_ = k8sClient.Delete(ctx, &pods.Items[i])
			}
		}

		By("cleaning up APIMAPIDeployment resources")
		deployments := &apimv1.APIMAPIDeploymentList{}
		_ = k8sClient.List(ctx, deployments)
		for i := range deployments.Items {
			if deployments.Items[i].Namespace == "default" {
				_ = k8sClient.Delete(ctx, &deployments.Items[i])
			}
		}

		By("cleaning up APIMAPI resources")
		apimAPIs := &apimv1.APIMAPIList{}
		_ = k8sClient.List(ctx, apimAPIs)
		for i := range apimAPIs.Items {
			if apimAPIs.Items[i].Namespace == "default" {
				_ = k8sClient.Delete(ctx, &apimAPIs.Items[i])
			}
		}

		By("cleaning up APIMService resources")
		apimServices := &apimv1.APIMServiceList{}
		_ = k8sClient.List(ctx, apimServices)
		for i := range apimServices.Items {
			if apimServices.Items[i].Namespace == "default" {
				_ = k8sClient.Delete(ctx, &apimServices.Items[i])
			}
		}
	})

	Context("When reconciling a resource", func() {
		It("should skip ReplicaSet without app label", func() {
			By("creating a ReplicaSet without app label")
			rs := &appsv1.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      replicaSetNamespacedName.Name,
					Namespace: replicaSetNamespacedName.Namespace,
					Labels:    map[string]string{}, // No app label
				},
				Spec: appsv1.ReplicaSetSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": appName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": appName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &ReplicaSetWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: replicaSetNamespacedName,
			})

			By("verifying that reconciliation skips gracefully")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should skip ReplicaSet scaled down to 0", func() {
			By("creating a ReplicaSet scaled down to 0")
			rs := &appsv1.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-replicaset-scaled-down",
					Namespace: "default",
					Labels: map[string]string{
						"app.kubernetes.io/name": appName,
					},
				},
				Spec: appsv1.ReplicaSetSpec{
					Replicas: int32Ptr(0), // Scaled down
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": appName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": appName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
			}()

			By("reconciling the resource")
			controllerReconciler := &ReplicaSetWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-replicaset-scaled-down",
					Namespace: "default",
				},
			})

			By("verifying that reconciliation skips gracefully")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should handle missing APIMAPI gracefully", func() {
			By("creating a ReplicaSet with app label but no APIMAPI")
			rs := &appsv1.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-replicaset-no-api",
					Namespace: "default",
					Labels: map[string]string{
						"app.kubernetes.io/name": "non-existent-app",
					},
				},
				Spec: appsv1.ReplicaSetSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "non-existent-app"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "non-existent-app"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())
			defer func() {
				Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
			}()

			By("reconciling the resource")
			controllerReconciler := &ReplicaSetWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-replicaset-no-api",
					Namespace: "default",
				},
			})

			By("verifying that missing APIMAPI is handled gracefully")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should handle deleted ReplicaSet gracefully", func() {
			By("creating a ReplicaSet")
			rs := &appsv1.ReplicaSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-replicaset-delete",
					Namespace: "default",
					Labels: map[string]string{
						"app.kubernetes.io/name": appName,
					},
				},
				Spec: appsv1.ReplicaSetSpec{
					Replicas: int32Ptr(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": appName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": appName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			By("deleting the ReplicaSet")
			Expect(k8sClient.Delete(ctx, rs)).To(Succeed())

			By("reconciling the deleted resource")
			controllerReconciler := &ReplicaSetWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-replicaset-delete",
					Namespace: "default",
				},
			})

			By("verifying that deletion is handled gracefully")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should create APIMAPIDeployment for selector-based APIMAPI without app label", func() {
			const selectorAPIName = "selector-api"

			By("creating a selector-based APIMAPI")
			selectorAPI := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:      selectorAPIName,
					Namespace: "default",
				},
				Spec: apimv1.APIMAPISpec{
					APIID:       "selector-api-id",
					APIMService: apimServiceName,
					Target: &apimv1.APIMAPITarget{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"team": "platform"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, selectorAPI)).To(Succeed())

			By("creating a ready ReplicaSet without the legacy app label")
			rs := createReplicaSet(ctx, "test-replicaset-selector", map[string]string{"team": "platform"}, map[string]string{"team": "platform"}, 1)
			createReadyPodForReplicaSet(ctx, rs, "test-pod-selector")

			By("reconciling the resource")
			controllerReconciler := &ReplicaSetWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      rs.Name,
					Namespace: rs.Namespace,
				},
			})

			By("verifying that a deployment was created for the selector-based APIMAPI")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			deployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: selectorAPIName, Namespace: "default"}, deployment)).To(Succeed())
			Expect(deployment.Spec.APIMAPIName).To(Equal(selectorAPIName))
			Expect(deployment.Spec.APIID).To(Equal("selector-api-id"))
		})

		It("should create deployments for both legacy and selector-based APIMAPIs", func() {
			const selectorAPIName = "selector-api"

			By("creating a selector-based APIMAPI that matches the same ReplicaSet as the legacy APIMAPI")
			selectorAPI := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{
					Name:      selectorAPIName,
					Namespace: "default",
				},
				Spec: apimv1.APIMAPISpec{
					APIID:       "selector-api-id",
					APIMService: apimServiceName,
					Target: &apimv1.APIMAPITarget{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app.kubernetes.io/name": appName},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, selectorAPI)).To(Succeed())

			By("creating a ready ReplicaSet that matches both APIs")
			rs := createReplicaSet(ctx, "test-replicaset-multi-match", map[string]string{"app.kubernetes.io/name": appName}, map[string]string{"app": appName}, 1)
			createReadyPodForReplicaSet(ctx, rs, "test-pod-multi-match")

			By("reconciling the resource")
			controllerReconciler := &ReplicaSetWatcherReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      rs.Name,
					Namespace: rs.Namespace,
				},
			})

			By("verifying that deployments were created for both matches")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			legacyDeployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: appName, Namespace: "default"}, legacyDeployment)).To(Succeed())
			Expect(legacyDeployment.Spec.APIMAPIName).To(Equal(appName))

			selectorDeployment := &apimv1.APIMAPIDeployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: selectorAPIName, Namespace: "default"}, selectorDeployment)).To(Succeed())
			Expect(selectorDeployment.Spec.APIMAPIName).To(Equal(selectorAPIName))
			Expect(selectorDeployment.Spec.APIID).To(Equal("selector-api-id"))
		})
	})
})

// int32Ptr returns a pointer to an int32 value
func int32Ptr(i int32) *int32 {
	return &i
}

func createReplicaSet(ctx context.Context, name string, replicaSetLabels map[string]string, podLabels map[string]string, replicas int32) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    replicaSetLabels,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: int32Ptr(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: podLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "test",
						Image: "nginx:latest",
					}},
				},
			},
		},
	}

	Expect(k8sClient.Create(ctx, rs)).To(Succeed())
	return rs
}

func createReadyPodForReplicaSet(ctx context.Context, rs *appsv1.ReplicaSet, podName string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: rs.Namespace,
			Labels:    rs.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "test",
				Image: "nginx:latest",
			}},
		},
	}

	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}
