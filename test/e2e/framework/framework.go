/*
Copyright 2018 The Kubernetes Authors.

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

package framework

import (
	"flag"
	"path"
	"time"

	"bytes"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"os/exec"
	"strings"
)

var kubeConfig = flag.String(clientcmd.RecommendedConfigPathFlag, os.Getenv(clientcmd.RecommendedConfigPathEnvVar), "Path to kubeconfig containing embedded authinfo.")
var kubectlPath = flag.String("kubectl-path", "kubectl", "The kubectl binary to use. For development, you might use 'cluster/kubectl.sh' here.")
var poseidonManifestPath = flag.String("poseidonManifestPath", "github.com/kubernetes-sigs/poseidon/deploy/poseidon-deployment.yaml", "The Poseidon deployment manifest to use.")
var firmamentManifestPath = flag.String("firmamentManifestPath", "github.com/kubernetes-sigs/poseidon/deploy/firmament-deployment.yaml", "The Firmament deployment manifest to use.")
var testNamespace = flag.String("testNamespace", "poseidon-test", "The namespace to use for test")

const (
	poseidonDeploymentName  = "poseidon"
	firmamentDeploymentName = "firmament-scheduler"
)

func init() {
	flag.Parse()
	fmt.Println(*kubeConfig, *kubectlPath, *poseidonManifestPath, *firmamentManifestPath)
	getKubeConfigFromEnv()
}

// Framework supports common operations used by e2e tests; it will keep a client & a namespace for you.
// Eventual goal is to merge this with integration test framework.
type Framework struct {
	BaseName  string
	ClientSet clientset.Interface
	Namespace *v1.Namespace
	TestingNS string
	Options   FrameworkOptions
}

type FrameworkOptions struct {
	ClientQPS   float32
	ClientBurst int
}

// NewDefaultFramework makes a new framework and sets up a BeforeEach/AfterEach for
// you (you can write additional before/after each functions).
func NewDefaultFramework(baseName string) *Framework {
	options := FrameworkOptions{
		ClientQPS:   20,
		ClientBurst: 50,
	}
	return NewFramework(baseName, options)
}

// NewFramework makes a new framework and sets up a BeforeEach/AfterEach
func NewFramework(baseName string, options FrameworkOptions) *Framework {
	f := &Framework{
		BaseName:  baseName,
		Options:   options,
		ClientSet: nil,
		TestingNS: *testNamespace,
	}
	BeforeSuite(f.BeforeEach)
	AfterSuite(f.AfterEach)
	return f
}

// BeforeEach gets a client and makes a namespace.
func (f *Framework) BeforeEach() {
	var err error
	if f.ClientSet == nil {
		var config *rest.Config
		var err error
		config, err = clientcmd.BuildConfigFromFlags("", *kubeConfig)
		if err != nil {
			panic(err)
		}
		cs, err := clientset.NewForConfig(config)
		if err != nil {
			panic(err)
		}
		f.ClientSet = cs

	}

	Logf("Posedion test are pointing to %v", *kubeConfig)

	_ = f.DeleteService(f.TestingNS, "poseidon")
	_ = f.DeleteService(f.TestingNS, "firmament-service")

	// TODO(shiv): We need to pass the cluster role from env
	_ = f.DeletePoseidonClusterRole("poseidon", f.TestingNS)

	// This is needed if we have a dirty test run which leaves the pods and deployments hanging
	_ = f.deleteNamespaceIfExist(f.TestingNS)
	_ = f.DeleteDeploymentIfExist(f.TestingNS, poseidonDeploymentName)
	_ = f.DeleteDeploymentIfExist(f.TestingNS, firmamentDeploymentName)

	f.Namespace, err = f.createNamespace(f.ClientSet)
	Expect(err).NotTo(HaveOccurred())

	Logf("After name space creation %v", f.Namespace)

	err = f.CreateFirmament()
	Expect(err).NotTo(HaveOccurred())

	err = f.CreatePoseidon()
	Expect(err).NotTo(HaveOccurred())

}

// AfterEach deletes the namespace, after reading its events.
func (f *Framework) AfterEach() {
	//delete ns
	var err error

	if f.ClientSet == nil {
		Expect(f.ClientSet).To(Not(Equal(nil)))
	}

	// Fetch Poseidon and Firmament logs before ending the test suite
	f.FetchLogsFromFirmament(f.TestingNS)
	f.FetchLogsFromPoseidon(f.TestingNS)
	Logf("Delete namespace called")
	err = f.deleteNamespace(f.TestingNS)
	Expect(err).NotTo(HaveOccurred())

	err = f.DeleteDeploymentIfExist(f.TestingNS, poseidonDeploymentName)
	Expect(err).NotTo(HaveOccurred())

	err = f.DeleteDeploymentIfExist(f.TestingNS, firmamentDeploymentName)
	Expect(err).NotTo(HaveOccurred())

}

// WaitForPodNotFound waits for the pod to be completely terminated (not "Get-able").
func (f *Framework) WaitForPodNotFound(podName string, timeout time.Duration) error {
	return waitForPodNotFoundInNamespace(f.ClientSet, podName, f.Namespace.Name, timeout)
}

// WaitForPodRunning waits for the pod to run in the namespace.
func (f *Framework) WaitForPodRunning(podName string) error {
	return WaitForPodNameRunningInNamespace(f.ClientSet, podName, f.Namespace.Name)
}

// WaitForPodRunningSlow waits for the pod to run in the namespace.
// It has a longer timeout then WaitForPodRunning (util.slowPodStartTimeout).
func (f *Framework) WaitForPodRunningSlow(podName string) error {
	return waitForPodRunningInNamespaceSlow(f.ClientSet, podName, f.Namespace.Name)
}

// WaitForPodNoLongerRunning waits for the pod to no longer be running in the namespace, for either
// success or failure.
func (f *Framework) WaitForPodNoLongerRunning(podName string) error {
	return WaitForPodNoLongerRunningInNamespace(f.ClientSet, podName, f.Namespace.Name)
}

// CreateFirmament create firmament deployment using kubectl
// TODO(shiv): We need to refrain from using 'kubectl' command from out tests.
// Refer issue: https://github.com/kubernetes/test-infra/issues/7901
func (f *Framework) CreateFirmament() error {
	outputStr, errorStr, err := f.KubectlExecCreate(*firmamentManifestPath)
	if err != nil {
		Logf("kubectl create firmamnet deployment command error string %v", errorStr)
		Logf("kubectl create firmamnet deployment command output string %v", outputStr)
		Logf("%v", err)
	}
	return err
}

// CreatePoseidon create firmament deployment using kubectl
// TODO(shiv): We need to refrain from using 'kubectl' command from out tests.
// Refer issue: https://github.com/kubernetes/test-infra/issues/7901
func (f *Framework) CreatePoseidon() error {
	outputStr, errorStr, err := f.KubectlExecCreate(*poseidonManifestPath)
	if err != nil {
		Logf("Command error string %v", errorStr)
		Logf("Command output string %v", outputStr)
		Logf("%v", err)
	}
	return err
}

// KubectlCmd runs the kubectl executable through the wrapper script.
func KubectlCmd(args ...string) *exec.Cmd {
	defaultArgs := []string{}

	if kubeConfig != nil {
		defaultArgs = append(defaultArgs, "--"+clientcmd.RecommendedConfigPathFlag+"="+*kubeConfig)

	}
	Logf("kubeConfig file in KubectlCmd %v %v", *kubeConfig, defaultArgs)
	kubectlArgs := append(defaultArgs, args...)
	cmd := exec.Command(*kubectlPath, kubectlArgs...)
	return cmd
}

func (f *Framework) KubectlExecCreate(manifestPath string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmdArgs := []string{
		fmt.Sprintf("create"),
		fmt.Sprintf("-f"),
		fmt.Sprintf("%v", manifestPath),
	}
	cmd := KubectlCmd(cmdArgs...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	Logf("Running '%s %s'", cmd.Path, strings.Join(cmdArgs, " "))
	err := cmd.Run()

	if err != nil {
		Logf("Unable to deploy %v %v", stdout.String(), stderr.String())
	}

	return stdout.String(), stderr.String(), err
}

func getKubeConfigFromEnv() {

	if *kubeConfig == "" {
		//read the config from the env
		*kubeConfig = path.Join(os.Getenv("HOME"), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)

	}
	Logf("Location of the kubeconfig file %v", *kubeConfig)
}
