package kubeadm

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/pkg/errors"
	versionutil "k8s.io/apimachinery/pkg/util/version"

	"github.com/aws/eks-anywhere-build-tooling/bottlerocket-bootstrap/pkg/utils"
)

func controlPlaneJoin() error {
	err := setHostName(kubeadmJoinFile)
	if err != nil {
		return errors.Wrap(err, "Error replacing hostname on kubeadm")
	}

	// start optional EBS initialization
	ebsInitControl := startEbsInit()

	cmd := exec.Command(kubeadmBinary, utils.LogVerbosity, "join", "phase", "control-plane-prepare", "all", "--config", kubeadmJoinFile)
	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "Error running command: %v", cmd)
	}

	cmd = exec.Command(kubeadmBinary, utils.LogVerbosity, "join", "phase", "kubelet-start", "--config", kubeadmJoinFile)
	if err := cmd.Start(); err != nil {
		return errors.Wrapf(err, "Error running command: %v", cmd)
	}

	err = killCmdAfterJoinFilesGeneration(cmd)
	if err != nil {
		return errors.Wrap(err, "Error waiting for worker join files")
	}

	dns, err := getDNSFromJoinConfig(kubeletConfigFile)
	if err != nil {
		return errors.Wrap(err, "Error getting api server")
	}

	apiServer, token, err := getBootstrapFromJoinConfig(kubeadmJoinFile)
	if err != nil {
		return errors.Wrap(err, "Error getting api server")
	}

	// Get CA
	b64CA, err := getEncodedCA()
	if err != nil {
		return errors.Wrap(err, "Error reading the ca data")
	}

	args := []string{
		"set",
		"kubernetes.api-server=" + apiServer,
		"kubernetes.cluster-certificate=" + b64CA,
		"kubernetes.cluster-dns-ip=" + dns,
		"kubernetes.bootstrap-token=" + token,
		"kubernetes.authentication-mode=tls",
		"kubernetes.standalone-mode=false",
	}

	kubeletTlsConfig := readKubeletTlsConfig(&RealFileReader{})
	if kubeletTlsConfig != nil {
		args = append(args, "settings.kubernetes.server-certificate="+kubeletTlsConfig.KubeletServingCert)
		args = append(args, "settings.kubernetes.server-key="+kubeletTlsConfig.KubeletServingPrivateKey)
	}

	cmd = exec.Command(utils.ApiclientBinary, args...)
	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "Error running command: %v", cmd)
	}

	err = waitForActiveKubelet()
	if err != nil {
		return errors.Wrap(err, "Error waiting for kubelet to come up")
	}

	kubeadmVersion, err := getKubeadmVersion()
	if err != nil {
		return errors.Wrapf(err, "getting kubeadm version")
	}

	if isEtcdExternal, err := isClusterWithExternalEtcd(kubeconfigPath); err != nil {
		return err
	} else if !isEtcdExternal {
		if err := joinLocalEtcd(kubeadmVersion); err != nil {
			return err
		}
	}

	// Migrate all static pods from this host-container to the bottlerocket host using the apiclient
	// now that etcd manifest is also created
	podDefinitions, err := utils.EnableStaticPods(staticPodManifestsPath)
	if err != nil {
		return errors.Wrap(err, "Error enabling static pods")
	}

	// Now that etcd is up and running, check for other pod liveness
	err = utils.WaitForPods(podDefinitions)
	if err != nil {
		return errors.Wrapf(err, "Error waiting for static pods to be up")
	}

	// Wait for Kubernetes API server to come up.
	localApiServerReadinessEndpoint, err := getLocalApiServerReadinessEndpoint()
	if err != nil {
		fmt.Printf("unable to get local apiserver readiness endpoint, falling back to localhost:6443. caused by: %s", err.Error())
		localApiServerReadinessEndpoint = "https://localhost:6443/healthz"
	}

	err = utils.WaitFor200(localApiServerReadinessEndpoint, 30*time.Second)
	if err != nil {
		return err
	}

	err = utils.WaitFor200(string(apiServer)+"/healthz", 30*time.Second)
	if err != nil {
		return err
	}

	k8s131Compare, err := kubeadmVersion.Compare("1.31.0")
	if err != nil {
		return errors.Wrap(err, "Error comparing kubeadm version with v1.31.0")
	}

	// finish kubeadm
	skipPhasesArgs := "preflight,control-plane-prepare,kubelet-start,control-plane-join/etcd"
	// 'kubelet-wait-bootstrap' phase was introduced only in 1.31, skip the phase for 1.31 and above
	if k8s131Compare != -1 {
		skipPhasesArgs = fmt.Sprintf("%s,%s", skipPhasesArgs, "kubelet-wait-bootstrap")
	}

	cmd = exec.Command(kubeadmBinary, utils.LogVerbosity, "join", "--skip-phases", skipPhasesArgs,
		"--config", kubeadmJoinFile)
	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "Error running command: %v", cmd)
	}

	if ebsInitControl != nil {
		checkEbsInit(ebsInitControl)
	}

	return nil
}

func joinLocalEtcd(version *versionutil.Version) error {
	k8s131Compare, err := version.Compare("1.31.0")
	if err != nil {
		return errors.Wrap(err, "Error comparing kubeadm version with v1.31.0")
	}

	var cmd *exec.Cmd
	joinCmdArgs := []string{utils.LogVerbosity, "join", "phase", "control-plane-join", "etcd", "--config", kubeadmJoinFile}
	// From K8s 1.31, the 'ControlPlaneKubeletLocalMode' feature gate is set to true by default.
	// Reference: https://github.com/kubernetes/kubernetes/pull/125582
	// Ideally we would want to run just the 'control-plane-join-etcd' phase introduced from above PR,
	// but kubeadm throws an error when we pass config file to the above phase, without the config
	// command hangs as it unable to find the required cluster-info
	// instead skip all the other phases in join so that only 'control-plane-join-etcd' phase runs
	if k8s131Compare != -1 {
		joinCmdArgs = []string{utils.LogVerbosity, "join", "--skip-phases", "preflight,control-plane-prepare,kubelet-start,control-plane-join,kubelet-wait-bootstrap,wait-control-plane", "--config", kubeadmJoinFile}
	}

	cmd = exec.Command(kubeadmBinary, joinCmdArgs...)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		return errors.Wrapf(err, "Error running command: %v", cmd)
	}

	// Get kubeadm to write out the manifest for etcd.
	// It will wait for etcd to start, which won't succeed because we need to set the static-pods in the BR api.
	etcdCheckFiles := []string{"/etc/kubernetes/manifests/etcd.yaml"}
	if err := utils.KillCmdAfterFilesGeneration(cmd, etcdCheckFiles); err != nil {
		return errors.Wrap(err, "Error waiting for etcd manifest files")
	}
	return nil
}
