package psa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	v1 "github.com/rancher/shepherd/clients/rancher/v1"
	"github.com/rancher/shepherd/extensions/clusters"
	"github.com/rancher/shepherd/extensions/defaults"
	extensionsworkloads "github.com/rancher/shepherd/extensions/workloads"
	wloads "github.com/rancher/shepherd/extensions/workloads"
	namegen "github.com/rancher/shepherd/pkg/namegenerator"
	"github.com/rancher/tests/actions/namespaces"
	"github.com/rancher/tests/actions/rbac"
	"github.com/rancher/tests/actions/workloads"
	appv1 "k8s.io/api/apps/v1"
	coreV1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kwait "k8s.io/apimachinery/pkg/util/wait"
)

const (
	pssRestrictedPolicy = "restricted"
	pssBaselinePolicy   = "baseline"
	pssPrivilegedPolicy = "privileged"
	psaWarn             = "pod-security.kubernetes.io/warn"
	psaAudit            = "pod-security.kubernetes.io/audit"
	psaEnforce          = "pod-security.kubernetes.io/enforce"
	psaRole             = "updatepsa"
	isCattleLabeled     = true
)

func getPSALabels(response *v1.SteveAPIObject, actualLabels map[string]string) map[string]string {
	expectedLabels := map[string]string{}

	for label := range response.Labels {
		if _, found := actualLabels[label]; found {
			expectedLabels[label] = actualLabels[label]
		}
	}
	return expectedLabels
}

func createDeploymentAndWait(steveclient *v1.Client, containerName string, image string, namespaceName string) (*v1.SteveAPIObject, error) {
	deploymentName := namegen.AppendRandomString("rbac-")
	containerTemplate := extensionsworkloads.NewContainer(containerName, image, coreV1.PullAlways, []coreV1.VolumeMount{}, []coreV1.EnvFromSource{}, nil, nil, nil)

	podTemplate := wloads.NewPodTemplate([]coreV1.Container{containerTemplate}, []coreV1.Volume{}, []coreV1.LocalObjectReference{}, nil, nil)
	deployment := wloads.NewDeploymentTemplate(deploymentName, namespaceName, podTemplate, isCattleLabeled, nil)

	deploymentResp, err := steveclient.SteveType(workloads.DeploymentSteveType).Create(deployment)
	if err != nil {
		return nil, err
	}
	err = kwait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
		deploymentResp, err := steveclient.SteveType(workloads.DeploymentSteveType).ByID(deployment.Namespace + "/" + deployment.Name)
		if err != nil {
			return false, err
		}
		deployment := &appv1.Deployment{}
		err = v1.ConvertToK8sType(deploymentResp.JSONResp, deployment)
		if err != nil {
			return false, err
		}
		status := deployment.Status.Conditions
		for _, statusCondition := range status {
			if strings.Contains(statusCondition.Message, "forbidden") {
				err = errors.New(statusCondition.Message)
				return false, err
			}
		}
		if *deployment.Spec.Replicas == deployment.Status.AvailableReplicas {
			return true, nil
		}
		return false, nil
	})
	return deploymentResp, err
}

func deletePSALabels(labels map[string]string) {
	for label := range labels {
		if strings.Contains(label, rbac.PSAWarnLabelKey) || strings.Contains(label, rbac.PSAAuditLabelKey) || strings.Contains(label, rbac.PSAEnforceLabelKey) {
			delete(labels, label)
		}
	}
}

func editPsactCluster(client *rancher.Client, clustername string, namespace string, psact string) (clusterType string, err error) {
	clusterID, err := clusters.GetClusterIDByName(client, clustername)
	if err != nil {
		return "", err
	}
	//Check if the downstream cluster is RKE2/K3S or RKE1
	if strings.Contains(clusterID, "c-m-") {
		clusterType = "RKE2K3S"
		clusterObj, existingSteveAPIObj, err := clusters.GetProvisioningClusterByName(client, clustername, namespace)
		if err != nil {
			return "", err
		}

		clusterObj.Spec.DefaultPodSecurityAdmissionConfigurationTemplateName = psact
		_, err = clusters.UpdateK3SRKE2Cluster(client, existingSteveAPIObj, clusterObj)
		if err != nil {
			return clusterType, err
		}
		updatedClusterObj, _, err := clusters.GetProvisioningClusterByName(client, clustername, namespace)
		if err != nil {
			return "", err
		}
		if updatedClusterObj.Spec.DefaultPodSecurityAdmissionConfigurationTemplateName != psact {
			errorMsg := "psact value was not changed, Expected: " + psact + ", Actual: " + updatedClusterObj.Spec.DefaultPodSecurityAdmissionConfigurationTemplateName
			return clusterType, errors.New(errorMsg)
		}
	} else {
		clusterType = "RKE"
		if psact == "" {
			psact = " "
		}
		existingCluster, err := client.Management.Cluster.ByID(clusterID)
		if err != nil {
			return "", err
		}

		updatedCluster := &management.Cluster{
			Name: existingCluster.Name,
			DefaultPodSecurityAdmissionConfigurationTemplateName: psact,
		}
		_, err = client.Management.Cluster.Update(existingCluster, updatedCluster)
		if err != nil {
			return clusterType, err
		}

		err = clusters.WaitForActiveRKE1Cluster(client, clusterID)
		if err != nil {
			return "", err
		}

		modifiedCluster, err := client.Management.Cluster.ByID(clusterID)
		if err != nil {
			return "", err
		}
		if psact == " " {
			psact = ""
		}
		if modifiedCluster.DefaultPodSecurityAdmissionConfigurationTemplateName != psact {
			errorMsg := "psact value was not changed, Expected: " + psact + ", Actual: " + modifiedCluster.DefaultPodSecurityAdmissionConfigurationTemplateName
			return clusterType, errors.New(errorMsg)
		}
	}
	return clusterType, nil
}

func getAndConvertNamespace(namespace *v1.SteveAPIObject, steveAdminClient *v1.Client) (*coreV1.Namespace, error) {
	getNSSteveObject, err := steveAdminClient.SteveType(namespaces.NamespaceSteveType).ByID(namespace.ID)
	if err != nil {
		return nil, err
	}
	namespaceObj := &coreV1.Namespace{}
	err = v1.ConvertToK8sType(getNSSteveObject.JSONResp, namespaceObj)
	if err != nil {
		return nil, err
	}
	return namespaceObj, nil
}

func createRole(client *rancher.Client, context string, roleName string, rules []management.PolicyRule) (role *management.RoleTemplate, err error) {
	role, err = client.Management.RoleTemplate.Create(
		&management.RoleTemplate{
			Context: context,
			Name:    roleName,
			Rules:   rules,
		})
	return

}

func createProject(client *rancher.Client, clusterID string) (*management.Project, error) {
	projectName := namegen.AppendRandomString("testproject-")
	projectConfig := &management.Project{
		ClusterID: clusterID,
		Name:      projectName,
	}
	createProject, err := client.Management.Project.Create(projectConfig)
	if err != nil {
		return nil, err
	}
	return createProject, nil
}

func getPSALabelsFromNamespace(namespace *coreV1.Namespace) map[string]string {
	psaLabels := map[string]string{}
	for key, value := range namespace.Labels {
		if strings.Contains(key, rbac.PSALabelKey) {
			psaLabels[key] = value
		}
	}
	return psaLabels
}

func generatePSALabels() map[string]string {
	return map[string]string{
		rbac.PSAEnforceLabelKey:        rbac.PSAPrivilegedPolicy,
		rbac.PSAEnforceVersionLabelKey: rbac.PSALatestValue,
		rbac.PSAWarnLabelKey:           rbac.PSAPrivilegedPolicy,
		rbac.PSAWarnVersionLabelKey:    rbac.PSALatestValue,
		rbac.PSAAuditLabelKey:          rbac.PSAPrivilegedPolicy,
		rbac.PSAAuditVersionLabelKey:   rbac.PSALatestValue,
	}
}

func createUpdatePSARoleTemplate(client *rancher.Client) (*v3.RoleTemplate, error) {
	updatePsaRules := []rbacv1.PolicyRule{
		{
			Verbs:     []string{rbac.UpdatePsaVerb},
			APIGroups: []string{rbac.ManagementAPIGroup},
			Resources: []string{rbac.ProjectResource},
		},
	}

	roleTemplateName := "namespaces-psa"
	displayName := "Manage PSA Labels"

	err := kwait.PollUntilContextTimeout(context.TODO(), defaults.FiveHundredMillisecondTimeout, defaults.TenSecondTimeout, false, func(ctx context.Context) (done bool, pollErr error) {
		_, err := client.WranglerContext.Mgmt.RoleTemplate().Get(roleTemplateName, metav1.GetOptions{})
		if err != nil && apierrors.IsNotFound(err) {
			return true, nil
		}

		err = client.WranglerContext.Mgmt.RoleTemplate().Delete(roleTemplateName, &metav1.DeleteOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to delete existing RoleTemplate: %w", err)
		}
		return false, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to wait for RoleTemplate deletion: %w", err)
	}

	roleTemplate := &v3.RoleTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleTemplateName,
		},
		Context:       rbac.ProjectContext,
		Rules:         updatePsaRules,
		DisplayName:   displayName,
		External:      false,
		ExternalRules: nil,
	}

	createdRoleTemplate, err := client.WranglerContext.Mgmt.RoleTemplate().Create(roleTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to create update-psa RoleTemplate: %w", err)
	}

	err = kwait.PollUntilContextTimeout(context.TODO(), defaults.FiveHundredMillisecondTimeout, defaults.TenSecondTimeout, false, func(ctx context.Context) (done bool, pollErr error) {
		_, err := client.WranglerContext.Mgmt.RoleTemplate().Get(roleTemplateName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get RoleTemplate after creation: %w", err)
		}

		return true, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed waiting for RoleTemplate creation: %w", err)
	}

	return createdRoleTemplate, nil
}
