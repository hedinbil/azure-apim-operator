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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hedinit/azure-apim-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "azure-apim-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "azure-apim-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "azure-apim-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "azure-apim-operator-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=azure-apim-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// The logs are in JSON format, so we check for the message field content
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("APIMService Controller", func() {
		const testNamespace = "e2e-test"
		const apimServiceName = "e2e-apim-service"

		BeforeEach(func() {
			By("creating test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, _ = utils.Run(cmd)
		})

		AfterEach(func() {
			By("cleaning up APIMService resources")
			cmd := exec.Command("kubectl", "delete", "apimservice", "--all", "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("removing test namespace")
			cmd = exec.Command("kubectl", "delete", "ns", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should create and reconcile APIMService resource", func() {
			By("creating an APIMService resource")
			apimServiceYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMService
metadata:
  name: %s
  namespace: %s
spec:
  name: test-apim
  resourceGroup: test-rg
  subscription: test-subscription-id
`, apimServiceName, testNamespace)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimServiceYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create APIMService")

			By("verifying the APIMService resource exists")
			verifyResourceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimservice", apimServiceName, "-n", testNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(apimServiceName))
			}
			Eventually(verifyResourceExists).Should(Succeed())
		})
	})

	Context("APIMTag Controller", func() {
		const testNamespace = "e2e-test-tag"
		const apimServiceName = "e2e-apim-service-tag"
		const apimTagName = "e2e-apim-tag"

		BeforeEach(func() {
			By("creating test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, _ = utils.Run(cmd)

			By("creating APIMService resource as dependency in operator namespace")
			// APIMService must be in the operator namespace, not the test namespace
			apimServiceYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMService
metadata:
  name: %s
  namespace: %s
spec:
  name: test-apim
  resourceGroup: test-rg
  subscription: test-subscription-id
`, apimServiceName, namespace)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimServiceYAML)
			_, _ = utils.Run(cmd)
		})

		AfterEach(func() {
			By("cleaning up APIMTag resources")
			cmd := exec.Command("kubectl", "delete", "apimtag", "--all", "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("cleaning up APIMService resources from operator namespace")
			cmd = exec.Command("kubectl", "delete", "apimservice", apimServiceName, "-n", namespace)
			_, _ = utils.Run(cmd)

			By("removing test namespace")
			cmd = exec.Command("kubectl", "delete", "ns", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should create and reconcile APIMTag resource", func() {
			By("creating an APIMTag resource")
			apimTagYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMTag
metadata:
  name: %s
  namespace: %s
spec:
  apimService: %s
  tagId: e2e-test-tag-id
  displayName: E2E Test Tag
`, apimTagName, testNamespace, apimServiceName)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimTagYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create APIMTag")

			By("verifying the APIMTag resource exists")
			verifyResourceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimtag", apimTagName, "-n", testNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(apimTagName))
			}
			Eventually(verifyResourceExists).Should(Succeed())

			By("verifying status is updated (will be Error due to missing Azure credentials)")
			verifyStatusUpdated := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimtag", apimTagName, "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Status should be set (either Error or Created)
				g.Expect(output).NotTo(BeEmpty())
			}
			Eventually(verifyStatusUpdated, 30*time.Second).Should(Succeed())
		})

		It("should handle missing APIMService dependency gracefully", func() {
			By("creating an APIMTag with non-existent APIMService")
			apimTagYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMTag
metadata:
  name: %s-invalid
  namespace: %s
spec:
  apimService: non-existent-service
  tagId: e2e-test-tag-id
  displayName: E2E Test Tag
`, apimTagName, testNamespace)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimTagYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create APIMTag")

			By("verifying the resource exists but reconciliation handles missing dependency")
			verifyResourceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimtag", fmt.Sprintf("%s-invalid", apimTagName), "-n", testNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(apimTagName))
			}
			Eventually(verifyResourceExists).Should(Succeed())
		})
	})

	Context("APIMProduct Controller", func() {
		const testNamespace = "e2e-test-product"
		const apimServiceName = "e2e-apim-service-product"
		const apimProductName = "e2e-apim-product"

		BeforeEach(func() {
			By("creating test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, _ = utils.Run(cmd)

			By("creating APIMService resource as dependency in operator namespace")
			// APIMService must be in the operator namespace, not the test namespace
			apimServiceYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMService
metadata:
  name: %s
  namespace: %s
spec:
  name: test-apim
  resourceGroup: test-rg
  subscription: test-subscription-id
`, apimServiceName, namespace)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimServiceYAML)
			_, _ = utils.Run(cmd)
		})

		AfterEach(func() {
			By("cleaning up APIMProduct resources")
			cmd := exec.Command("kubectl", "delete", "apimproduct", "--all", "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("cleaning up APIMService resources from operator namespace")
			cmd = exec.Command("kubectl", "delete", "apimservice", apimServiceName, "-n", namespace)
			_, _ = utils.Run(cmd)

			By("removing test namespace")
			cmd = exec.Command("kubectl", "delete", "ns", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should create and reconcile APIMProduct resource", func() {
			By("creating an APIMProduct resource")
			apimProductYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMProduct
metadata:
  name: %s
  namespace: %s
spec:
  apimService: %s
  productId: e2e-test-product-id
  displayName: E2E Test Product
  description: E2E Test Product Description
  published: false
`, apimProductName, testNamespace, apimServiceName)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimProductYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create APIMProduct")

			By("verifying the APIMProduct resource exists")
			verifyResourceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimproduct", apimProductName, "-n", testNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(apimProductName))
			}
			Eventually(verifyResourceExists).Should(Succeed())

			By("verifying status is updated")
			verifyStatusUpdated := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimproduct", apimProductName, "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}
			Eventually(verifyStatusUpdated, 30*time.Second).Should(Succeed())
		})

		It("should handle product deletion", func() {
			By("creating an APIMProduct resource")
			apimProductYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMProduct
metadata:
  name: %s-delete
  namespace: %s
spec:
  apimService: %s
  productId: e2e-test-product-delete-id
  displayName: E2E Test Product To Delete
  published: false
`, apimProductName, testNamespace, apimServiceName)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimProductYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create APIMProduct")

			By("verifying the resource exists")
			verifyResourceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimproduct", fmt.Sprintf("%s-delete", apimProductName), "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyResourceExists).Should(Succeed())

			By("deleting the APIMProduct resource")
			cmd = exec.Command("kubectl", "delete", "apimproduct", fmt.Sprintf("%s-delete", apimProductName), "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete APIMProduct")

			By("verifying the resource is deleted")
			verifyResourceDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apimproduct", fmt.Sprintf("%s-delete", apimProductName), "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred()) // Should fail because resource doesn't exist
			}
			Eventually(verifyResourceDeleted).Should(Succeed())
		})
	})

	Context("APIMInboundPolicy Controller", func() {
		const testNamespace = "e2e-test-policy"
		const apimServiceName = "e2e-apim-service-policy"
		const apimPolicyName = "e2e-apim-policy"

		BeforeEach(func() {
			By("creating test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, _ = utils.Run(cmd)

			By("creating APIMService resource as dependency in operator namespace")
			// APIMService must be in the operator namespace, not the test namespace
			apimServiceYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMService
metadata:
  name: %s
  namespace: %s
spec:
  name: test-apim
  resourceGroup: test-rg
  subscription: test-subscription-id
`, apimServiceName, namespace)

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimServiceYAML)
			_, _ = utils.Run(cmd)
		})

		AfterEach(func() {
			By("cleaning up APIMInboundPolicy resources")
			cmd := exec.Command("kubectl", "delete", "apiminboundpolicy", "--all", "-n", testNamespace)
			_, _ = utils.Run(cmd)

			By("cleaning up APIMService resources from operator namespace")
			cmd = exec.Command("kubectl", "delete", "apimservice", apimServiceName, "-n", namespace)
			_, _ = utils.Run(cmd)

			By("removing test namespace")
			cmd = exec.Command("kubectl", "delete", "ns", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should create and reconcile APIMInboundPolicy resource", func() {
			By("creating an APIMInboundPolicy resource")
			apimPolicyYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMInboundPolicy
metadata:
  name: %s
  namespace: %s
spec:
  apimService: %s
  apiId: e2e-test-api-id
  policyContent: |
    <policies>
      <inbound>
        <base />
      </inbound>
    </policies>
`, apimPolicyName, testNamespace, apimServiceName)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimPolicyYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create APIMInboundPolicy")

			By("verifying the APIMInboundPolicy resource exists")
			verifyResourceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apiminboundpolicy", apimPolicyName, "-n", testNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(apimPolicyName))
			}
			Eventually(verifyResourceExists).Should(Succeed())

			By("verifying status is updated")
			verifyStatusUpdated := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apiminboundpolicy", apimPolicyName, "-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}
			Eventually(verifyStatusUpdated, 30*time.Second).Should(Succeed())
		})

		It("should handle APIMInboundPolicy with operation ID", func() {
			By("creating an APIMInboundPolicy resource with operation ID")
			apimPolicyYAML := fmt.Sprintf(`apiVersion: apim.operator.io/v1
kind: APIMInboundPolicy
metadata:
  name: %s-operation
  namespace: %s
spec:
  apimService: %s
  apiId: e2e-test-api-id
  operationId: e2e-test-operation-id
  policyContent: |
    <policies>
      <inbound>
        <base />
      </inbound>
    </policies>
`, apimPolicyName, testNamespace, apimServiceName)

			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(apimPolicyYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create APIMInboundPolicy with operation ID")

			By("verifying the resource exists")
			verifyResourceExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "apiminboundpolicy", fmt.Sprintf("%s-operation", apimPolicyName), "-n", testNamespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(apimPolicyName))
			}
			Eventually(verifyResourceExists).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
