package operator

import (
	"fmt"
	"reflect"

	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-kube-apiserver-operator/pkg/apis/kubeapiserver/v1alpha1"
	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/v311_00_assets"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1alpha1helpers"
)

// createInstallerController_v311_00_to_latest takes care of creating content for the static pods to deploy.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func createInstallerController_v311_00_to_latest(c InstallerController, operatorConfig *v1alpha1.KubeApiserverOperatorConfig) (bool, error) {
	operatorConfigOriginal := operatorConfig.DeepCopy()

	for i := range operatorConfig.Status.TargetKubeletStates {
		var currKubeletState *v1alpha1.KubeletState
		var prevKubeletState *v1alpha1.KubeletState
		currKubeletState = &operatorConfig.Status.TargetKubeletStates[i]
		if i > 0 {
			prevKubeletState = &operatorConfig.Status.TargetKubeletStates[i-1]
		}

		// if we are in a transition, check to see if our installer pod completed
		if currKubeletState.CurrentDeploymentID != currKubeletState.TargetDeploymentID {
			// TODO check to see if our installer pod completed.  Success or failure there indicates whether we should be failed.
			newCurrKubeletState, err := checkIfInstallComplete(c.kubeClient, currKubeletState)
			if err != nil {
				return true, err
			}

			// if we make a change to this status, we want to write it out to the API before we commence work on the next kubelet.
			// it's an extra write/read, but it makes the state debuggable from outside this process
			if !equality.Semantic.DeepEqual(newCurrKubeletState, currKubeletState) {
				glog.Infof("%q moving to %v", currKubeletState.NodeName, spew.Sdump(*newCurrKubeletState))
				operatorConfig.Status.TargetKubeletStates[i] = *newCurrKubeletState
				if !reflect.DeepEqual(operatorConfigOriginal, operatorConfig) {
					_, updateError := c.operatorConfigClient.KubeApiserverOperatorConfigs().UpdateStatus(operatorConfig)
					return false, updateError
				}
			}
			break
		}

		deploymentIDToStart := getDeploymentIDToStart(currKubeletState, prevKubeletState, operatorConfig)
		if deploymentIDToStart == 0 {
			glog.V(4).Infof("%q does not need update", currKubeletState.NodeName)
			continue
		}
		glog.Infof("%q needs to deploy to %d", currKubeletState.NodeName, deploymentIDToStart)

		// we need to start a deployment create a pod that will lay down the static pod resources
		newCurrKubeletState, err := createInstallerPod(c.kubeClient, currKubeletState, operatorConfig, deploymentIDToStart)
		if err != nil {
			return true, err
		}
		// if we make a change to this status, we want to write it out to the API before we commence work on the next kubelet.
		// it's an extra write/read, but it makes the state debuggable from outside this process
		if !equality.Semantic.DeepEqual(newCurrKubeletState, currKubeletState) {
			glog.Infof("%q moving to %v", currKubeletState.NodeName, spew.Sdump(*newCurrKubeletState))
			operatorConfig.Status.TargetKubeletStates[i] = *newCurrKubeletState
			if !reflect.DeepEqual(operatorConfigOriginal, operatorConfig) {
				_, updateError := c.operatorConfigClient.KubeApiserverOperatorConfigs().UpdateStatus(operatorConfig)
				return false, updateError
			}
		}
		break
	}

	v1alpha1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1alpha1.OperatorCondition{
		Type:   "InstallerControllerFailing",
		Status: operatorv1alpha1.ConditionFalse,
	})
	if !reflect.DeepEqual(operatorConfigOriginal, operatorConfig) {
		_, updateError := c.operatorConfigClient.KubeApiserverOperatorConfigs().UpdateStatus(operatorConfig)
		if updateError != nil {
			return true, updateError
		}
	}

	return false, nil
}

// checkIfInstallComplete returns the new KubeletState or and error
func checkIfInstallComplete(c kubernetes.Interface, currKubeletState *v1alpha1.KubeletState) (*v1alpha1.KubeletState, error) {
	ret := currKubeletState.DeepCopy()
	installerPod, err := c.CoreV1().Pods(targetNamespaceName).Get(getInstallerPodName(currKubeletState.TargetDeploymentID, currKubeletState.NodeName), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ret.LastFailedDeploymentID = currKubeletState.TargetDeploymentID
		return ret, nil
	}
	if err != nil {
		return nil, err
	}
	switch installerPod.Status.Phase {
	case corev1.PodSucceeded:
		ret.CurrentDeploymentID = currKubeletState.TargetDeploymentID
		ret.TargetDeploymentID = 0
		ret.LastFailedDeploymentID = 0
	case corev1.PodFailed:
		ret.TargetDeploymentID = 0
		ret.LastFailedDeploymentID = currKubeletState.TargetDeploymentID
	}

	return ret, nil
}

// getDeploymentIDToStart returns the deploymentID we need to start or zero if none
func getDeploymentIDToStart(currKubeletState, prevKubeletState *v1alpha1.KubeletState, operatorConfig *v1alpha1.KubeApiserverOperatorConfig) int32 {
	if prevKubeletState == nil {
		currentAtLatest := currKubeletState.CurrentDeploymentID == operatorConfig.Status.LatestDeploymentID
		failedAtLatest := currKubeletState.LastFailedDeploymentID == operatorConfig.Status.LatestDeploymentID
		if !currentAtLatest && !failedAtLatest {
			return operatorConfig.Status.LatestDeploymentID
		}
	}

	prevInTransition := prevKubeletState.CurrentDeploymentID != prevKubeletState.TargetDeploymentID
	if prevInTransition {
		return 0
	}

	prevAhead := currKubeletState.CurrentDeploymentID > currKubeletState.CurrentDeploymentID
	failedAtPrev := currKubeletState.LastFailedDeploymentID == prevKubeletState.CurrentDeploymentID
	if prevAhead && !failedAtPrev {
		return currKubeletState.CurrentDeploymentID
	}

	return 0
}

func getInstallerPodName(deploymentID int32, nodeName string) string {
	return fmt.Sprintf("installer-%d-%s", deploymentID, nodeName)
}

// createInstallerPod creates the installer pod with the secrets required to
func createInstallerPod(c kubernetes.Interface, currKubeletState *v1alpha1.KubeletState, operatorConfig *v1alpha1.KubeApiserverOperatorConfig, deploymentID int32) (*v1alpha1.KubeletState, error) {
	required := resourceread.ReadPodV1OrDie(v311_00_assets.MustAsset("v3.11.0/kube-apiserver/installer-pod.yaml"))
	switch corev1.PullPolicy(operatorConfig.Spec.ImagePullPolicy) {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		required.Spec.Containers[0].ImagePullPolicy = corev1.PullPolicy(operatorConfig.Spec.ImagePullPolicy)
	case "":
	default:
		return nil, fmt.Errorf("invalid imagePullPolicy specified: %v", operatorConfig.Spec.ImagePullPolicy)
	}
	required.Name = getInstallerPodName(deploymentID, currKubeletState.NodeName)
	required.Spec.NodeName = currKubeletState.NodeName
	required.Spec.Containers[0].Image = operatorConfig.Spec.ImagePullSpec
	required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-v=%d", operatorConfig.Spec.Logging.Level))
	required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-deployment-id=%d", deploymentID))
	required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-namespace=%q", targetNamespaceName))
	required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-pod=%q", deploymentConfigMaps_v311_00_to_latest[0]))
	required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-resource-dir=%q", "/etc/kubernetes/static-pod-resources"))
	required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-pod-manifest-dir=%q", "/etc/kubernetes/manifests"))
	for _, name := range deploymentConfigMaps_v311_00_to_latest[1:] {
		required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-configmaps=%q", nameFor(name, deploymentID)))
	}
	for _, name := range deploymentSecrets_v311_00_to_latest {
		required.Spec.Containers[0].Args = append(required.Spec.Containers[0].Args, fmt.Sprintf("-secrets=%q", nameFor(name, deploymentID)))
	}

	if _, err := c.CoreV1().Pods(targetNamespaceName).Create(required); err != nil {
		glog.Errorf("failed to create pod for %q: %v", currKubeletState.NodeName, resourceread.WritePodV1OrDie(required))
		return nil, err
	}

	ret := currKubeletState.DeepCopy()
	ret.TargetDeploymentID = deploymentID

	return ret, nil
}
