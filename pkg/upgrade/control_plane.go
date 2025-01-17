// Copyright 2019 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	kubernetes2 "github.com/vmware/cluster-api-upgrade-tool/pkg/internal/kubernetes"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	bootstrapv1 "sigs.k8s.io/cluster-api-bootstrap-provider-kubeadm/api/v1alpha2"
	kubeadmv1beta1 "sigs.k8s.io/cluster-api-bootstrap-provider-kubeadm/kubeadm/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha2"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	etcdCACertFile = "/etc/kubernetes/pki/etcd/ca.crt"
	etcdCertFile   = "/etc/kubernetes/pki/etcd/peer.crt"
	etcdKeyFile    = "/etc/kubernetes/pki/etcd/peer.key"

	// annotationPrefix is the prefix for all annotations managed by this tool.
	annotationPrefix = "upgrade.cluster-api.vmware.com/"

	// AnnotationUpgradeID is the annotation key for an upgrade's identifier.
	AnnotationUpgradeID = annotationPrefix + "id"
)

var unsetVersion semver.Version

type ControlPlaneUpgrader struct {
	log                     logr.Logger
	userVersion             semver.Version
	desiredVersion          semver.Version
	clusterNamespace        string
	clusterName             string
	managementClusterClient ctrlclient.Client
	targetRestConfig        *rest.Config
	targetKubernetesClient  kubernetes.Interface
	providerIDsToNodes      map[string]*v1.Node
	imageField, imageID     string
	upgradeID               string
	oldNodeToEtcdMember     map[string]string
	secretsUpdated          bool
}

func NewControlPlaneUpgrader(log logr.Logger, config Config) (*ControlPlaneUpgrader, error) {
	// Validations
	if config.KubernetesVersion == "" {
		return nil, errors.New("kubernetes version is required")
	}
	if (config.MachineUpdates.Image.ID == "" && config.MachineUpdates.Image.Field != "") ||
		(config.MachineUpdates.Image.ID != "" && config.MachineUpdates.Image.Field == "") {
		return nil, errors.New("when specifying image id, image field is required (and vice versa)")
	}
	if !upgradeIDInputRegex.MatchString(config.UpgradeID) {
		return nil, errors.New("upgrade ID must be a timestamp containing only digits")
	}

	var userVersion, desiredVersion semver.Version

	v, err := semver.ParseTolerant(config.KubernetesVersion)
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing kubernetes version %q", config.KubernetesVersion)
	}
	userVersion = v
	desiredVersion = v

	managementClusterClient, err := kubernetes2.NewClient(
		kubernetes2.KubeConfigPath(config.ManagementCluster.Kubeconfig),
		kubernetes2.KubeConfigContext(config.ManagementCluster.Context),
	)
	if err != nil {
		return nil, err
	}

	log.Info("Retrieving cluster from management cluster", "cluster-namespace", config.TargetCluster.Namespace, "cluster-name", config.TargetCluster.Name)
	cluster := &clusterv1.Cluster{}
	err = managementClusterClient.Get(context.TODO(), ctrlclient.ObjectKey{Namespace: config.TargetCluster.Namespace, Name: config.TargetCluster.Name}, cluster)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	kc, err := kubeconfig.FromSecret(managementClusterClient, cluster)
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving cluster kubeconfig secret")
	}
	targetRestConfig, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		return nil, err
	}
	if targetRestConfig == nil {
		return nil, errors.New("could not get a kubeconfig for your target cluster")
	}

	log.Info("Creating target kubernetes client")
	targetKubernetesClient, err := kubernetes.NewForConfig(targetRestConfig)
	if err != nil {
		return nil, errors.Wrap(err, "error creating target cluster client")
	}

	if config.UpgradeID == "" {
		config.UpgradeID = fmt.Sprintf("%d", time.Now().Unix())
	}

	infoMessage := fmt.Sprintf("Rerun with `--upgrade-id=%s` if this upgrade fails midway and you want to retry", config.UpgradeID)
	log.Info(infoMessage)

	return &ControlPlaneUpgrader{
		log:                     log,
		userVersion:             userVersion,
		desiredVersion:          desiredVersion,
		clusterNamespace:        config.TargetCluster.Namespace,
		clusterName:             config.TargetCluster.Name,
		managementClusterClient: managementClusterClient,
		targetRestConfig:        targetRestConfig,
		targetKubernetesClient:  targetKubernetesClient,
		imageField:              config.MachineUpdates.Image.Field,
		imageID:                 config.MachineUpdates.Image.ID,
		upgradeID:               config.UpgradeID,
	}, nil
}

// Upgrade does the upgrading of the control plane.
func (u *ControlPlaneUpgrader) Upgrade() error {
	machines, err := u.listMachines()
	if err != nil {
		return err
	}

	if len(machines) == 0 {
		return errors.New("Found 0 control plane machines")
	}

	min, max, err := u.minMaxControlPlaneVersions(machines)
	if err != nil {
		return errors.Wrap(err, "error determining current control plane versions")
	}

	// default the desired version if the user did not specify it
	if unsetVersion.EQ(u.userVersion) {
		u.desiredVersion = max
	}

	if isMinorVersionUpgrade(min, u.desiredVersion) {
		err = u.updateKubeletConfigMapIfNeeded(u.desiredVersion)
		if err != nil {
			return err
		}

		err = u.updateKubeletRbacIfNeeded(u.desiredVersion)
		if err != nil {
			return err
		}
	}

	u.log.Info("Checking etcd health")
	if err := u.etcdClusterHealthCheck(time.Minute * 1); err != nil {
		return err
	}

	u.log.Info("Updating provider IDs to nodes")
	if err := u.UpdateProviderIDsToNodes(); err != nil {
		return err
	}

	u.log.Info("Updating kubernetes version")
	if err := u.updateAndUploadKubeadmKubernetesVersion(); err != nil {
		return err
	}

	u.log.Info("Updating machines")
	if err := u.updateMachines(machines); err != nil {
		return err
	}

	u.log.Info("Removing upgrade annotations")
	for _, m := range machines {
		var replacement clusterv1.Machine
		replacementName := generateReplacementMachineName(m.Name, u.upgradeID)

		key := ctrlclient.ObjectKey{
			Namespace: m.Namespace,
			Name:      replacementName,
		}

		if err := u.managementClusterClient.Get(context.TODO(), key, &replacement); err != nil {
			return errors.Wrapf(err, "error getting machine %s", key.String())
		}

		helper, err := patch.NewHelper(replacement.DeepCopy(), u.managementClusterClient)
		if err != nil {
			return err
		}

		delete(replacement.Annotations, AnnotationUpgradeID)

		if err := helper.Patch(context.TODO(), &replacement); err != nil {
			return err
		}
	}

	return nil
}

func isMinorVersionUpgrade(base, update semver.Version) bool {
	return base.Major == update.Major && base.Minor < update.Minor
}

func (u *ControlPlaneUpgrader) minMaxControlPlaneVersions(machines []*clusterv1.Machine) (semver.Version, semver.Version, error) {
	var min, max semver.Version

	for _, machine := range machines {
		if machine.Spec.Version == nil {
			return semver.Version{}, semver.Version{}, errors.Errorf("nil control plane version for machine %s/%s", machine.Namespace, machine.Name)
		}
		if *machine.Spec.Version != "" {
			machineVersion, err := semver.ParseTolerant(*machine.Spec.Version)
			if err != nil {
				return min, max, errors.Wrapf(err, "invalid control plane version %q for machine %s/%s", *machine.Spec.Version, machine.Namespace, machine.Name)
			}
			if min.EQ(unsetVersion) || machineVersion.LT(min) {
				min = machineVersion
			}
			if max.EQ(unsetVersion) || machineVersion.GT(max) {
				max = machineVersion
			}
		}
	}

	return min, max, nil
}

func (u *ControlPlaneUpgrader) updateKubeletConfigMapIfNeeded(version semver.Version) error {
	// Check if the desired configmap already exists
	desiredKubeletConfigMapName := fmt.Sprintf("kubelet-config-%d.%d", version.Major, version.Minor)
	_, err := u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Get(desiredKubeletConfigMapName, metav1.GetOptions{})
	if err == nil {
		u.log.Info("kubelet configmap already exists", "configMapName", desiredKubeletConfigMapName)
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error determining if configmap %s exists", desiredKubeletConfigMapName)
	}

	// If we get here, we have to make the configmap
	previousMinorVersionKubeletConfigMapName := fmt.Sprintf("kubelet-config-%d.%d", version.Major, version.Minor-1)
	cm, err := u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Get(previousMinorVersionKubeletConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errors.Errorf("unable to find current kubelet configmap %s", previousMinorVersionKubeletConfigMapName)
	}
	cm.Name = desiredKubeletConfigMapName
	cm.ResourceVersion = ""

	_, err = u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Create(cm)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrapf(err, "error creating configmap %s", desiredKubeletConfigMapName)
	}

	return nil
}

func (u *ControlPlaneUpgrader) updateKubeletRbacIfNeeded(version semver.Version) error {
	majorMinor := fmt.Sprintf("%d.%d", version.Major, version.Minor)
	roleName := fmt.Sprintf("kubeadm:kubelet-config-%s", majorMinor)

	_, err := u.targetKubernetesClient.RbacV1().Roles("kube-system").Get(roleName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		newRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "kube-system",
				Name:      roleName,
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:         []string{"get"},
					APIGroups:     []string{""},
					Resources:     []string{"configmaps"},
					ResourceNames: []string{fmt.Sprintf("kubelet-config-%s", majorMinor)},
				},
			},
		}

		_, err := u.targetKubernetesClient.RbacV1().Roles("kube-system").Create(newRole)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "error creating role %s", roleName)
		}
	} else if err != nil {
		return errors.Wrapf(err, "error determining if role %s exists", roleName)
	}

	_, err = u.targetKubernetesClient.RbacV1().RoleBindings("kube-system").Get(roleName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		newRoleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "kube-system",
				Name:      roleName,
			},
			Subjects: []rbacv1.Subject{
				{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "Group",
					Name:     "system:nodes",
				},
				{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "Group",
					Name:     "system:bootstrappers:kubeadm:default-node-token",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     roleName,
			},
		}

		_, err = u.targetKubernetesClient.RbacV1().RoleBindings("kube-system").Create(newRoleBinding)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "error creating rolebinding %s", roleName)
		}
	} else if err != nil {
		return errors.Wrapf(err, "error determining if rolebinding %s exists", roleName)
	}

	return nil
}

func (u *ControlPlaneUpgrader) etcdClusterHealthCheck(timeout time.Duration) error {
	members, err := u.listEtcdMembers(timeout)
	if err != nil {
		return err
	}

	var endpoints []string
	for _, member := range members {
		endpoints = append(endpoints, member.ClientURLs...)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), timeout)
	defer cancel()

	// TODO: we can switch back to using --cluster instead of --endpoints when we no longer need to support etcd 3.2
	// (which is the version kubeadm installs for Kubernetes v1.13.x). kubeadm switched to etcd 3.3 with v1.14.x.

	// TODO: use '-w json' when it's in the minimum supported etcd version.
	_, _, err = u.etcdctl(ctx, "endpoint health --endpoints", strings.Join(endpoints, ","))
	return err
}

func (u *ControlPlaneUpgrader) updateMachine(replacementKey ctrlclient.ObjectKey, machine *clusterv1.Machine) error {
	log := u.log.WithValues(
		"machine", fmt.Sprintf("%s/%s", machine.Namespace, machine.Name),
		"replacement", replacementKey.String(),
	)

	originalProviderID, err := noderefutil.NewProviderID(*machine.Spec.ProviderID)
	if err != nil {
		return err
	}
	log.Info("Determined provider id for machine", "provider-id", originalProviderID)

	oldNode := u.GetNodeFromProviderID(originalProviderID.ID())
	if oldNode == nil {
		u.log.Info("Couldn't retrieve oldNode", "id", originalProviderID.String())
		return fmt.Errorf("unknown previous node %q", originalProviderID.String())
	}

	oldHostName := hostnameForNode(oldNode)
	log.Info("Determined node hostname for machine", "node", oldNode.Name, "hostname", oldHostName)

	log.Info("Checking if we need to create a new machine")
	replacementRef := v1.ObjectReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Machine",
		Namespace:  replacementKey.Namespace,
		Name:       replacementKey.Name,
	}
	exists, err := u.resourceExists(replacementRef)
	if err != nil {
		return err
	}

	var replacementMachine *clusterv1.Machine
	if !exists {
		log.Info("New machine does not exist - need to create a new one")
		replacementMachine = machine.DeepCopy()

		// have to clear this out so we can create a new machine
		replacementMachine.ResourceVersion = ""

		// have to clear this out so the new machine can get its own provider id set
		replacementMachine.Spec.ProviderID = nil

		// Use the new, generated replacement machine name for all the things
		replacementMachine.Name = replacementKey.Name
		replacementMachine.Spec.InfrastructureRef.Name = replacementKey.Name
		replacementMachine.Spec.Bootstrap.Data = nil
		replacementMachine.Spec.Bootstrap.ConfigRef.Name = replacementKey.Name

		desiredVersion := u.desiredVersion.String()
		replacementMachine.Spec.Version = &desiredVersion

		log.Info("Creating new machine")
		if err := u.managementClusterClient.Create(context.TODO(), replacementMachine); err != nil {
			return errors.Wrapf(err, "Error creating machine: %s", replacementMachine.Name)
		}
		log.Info("Create succeeded")
	} else {
		log.Info("New machine exists - retrieving from server")
		replacementMachine = new(clusterv1.Machine)
		if err := u.managementClusterClient.Get(context.TODO(), replacementKey, replacementMachine); err != nil {
			return errors.Wrapf(err, "error getting replacement machine %s", replacementKey.String())
		}
	}

	// TODO extract timeout as a configurable constant
	newProviderID, err := u.waitForProviderID(u.clusterNamespace, replacementKey.Name, 15*time.Minute)
	if err != nil {
		return err
	}
	// TODO extract timeout as a configurable constant
	node, err := u.waitForMatchingNode(newProviderID, 15*time.Minute)
	if err != nil {
		return err
	}
	// TODO extract timeout as a configurable constant
	if err := u.waitForNodeReady(node, 15*time.Minute); err != nil {
		return err
	}

	// This used to happen when a new machine was created as a side effect. Must still update the mapping.
	if err := u.UpdateProviderIDsToNodes(); err != nil {
		return err
	}

	// Delete the etcd member, if necessary
	oldEtcdMemberID := u.oldNodeToEtcdMember[oldHostName]
	if oldEtcdMemberID != "" {
		// TODO make timeout the last arg, for consistency (or pass in a ctx?)
		err = u.deleteEtcdMember(time.Minute*1, oldEtcdMemberID)
		if err != nil {
			return errors.Wrapf(err, "unable to delete old etcd member %s", oldEtcdMemberID)
		}
	}

	u.log.Info("Deleting existing machine", "namespace", machine.Namespace, "name", machine.Name)
	// TODO plumb a context down to here instead of using TODO
	if err := u.managementClusterClient.Delete(context.TODO(), machine); err != nil {
		return errors.Wrapf(err, "error deleting machine %s/%s", machine.Namespace, machine.Name)
	}

	return nil
}

func (u *ControlPlaneUpgrader) updateMachines(machines []*clusterv1.Machine) error {
	// save all etcd member id corresponding to node before upgrade starts
	err := u.oldNodeToEtcdMemberId(time.Minute * 1)
	if err != nil {
		return err
	}

	for _, machine := range machines {
		log := u.log.WithValues(
			"machine", fmt.Sprintf("%s/%s", machine.Namespace, machine.Name),
			"upgrade-id", u.upgradeID,
		)

		if machine.Spec.ProviderID == nil {
			log.Info("unable to upgrade machine as it has no spec.providerID")
			// TODO record event/annotation?
			continue
		}

		annotations := machine.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
			machine.SetAnnotations(annotations)
		}

		// Add upgrade ID if it isn't there
		if annotations[AnnotationUpgradeID] == "" {
			helper, err := patch.NewHelper(machine.DeepCopy(), u.managementClusterClient)
			if err != nil {
				// TODO should we do anything else?
				log.Error(err, "error creating patch helper for machine (add upgrade id)")
				continue
			}

			machine.Annotations[AnnotationUpgradeID] = u.upgradeID

			log.Info("Storing upgrade ID on machine")

			if err := helper.Patch(context.TODO(), machine); err != nil {
				// TODO should we do anything else?
				log.Error(err, "error patching machine (add upgrade id)")
				continue
			}
		}

		// Don't process a mismatching upgrade ID
		if annotations[AnnotationUpgradeID] != u.upgradeID {
			// TODO record that we're unable to upgrade because the ID is a mismatch (annotation? event?)
			log.Info("Unable to upgrade machine - mismatching upgrade id", "machine-upgrade-id", annotations[AnnotationUpgradeID])
			continue
		}

		// Skip if this is a replacement machine for the current upgrade
		if strings.HasSuffix(machine.Name, upgradeSuffix(u.upgradeID)) {
			log.Info("Skipping machine as it is a replacement machine for the in-process upgrade")
			continue
		}

		// TODO skip if the bootstrap ref is not a KubeadmConfig

		replacementMachineName := generateReplacementMachineName(machine.Name, u.upgradeID)

		replacementKey := ctrlclient.ObjectKey{
			Namespace: u.clusterNamespace,
			Name:      replacementMachineName,
		}

		log.Info("Updating infrastructure reference",
			"api-version", machine.Spec.InfrastructureRef.APIVersion,
			"kind", machine.Spec.InfrastructureRef.Kind,
			"name", machine.Spec.InfrastructureRef.Name,
		)
		if err := u.updateInfrastructureReference(replacementKey, machine.Spec.InfrastructureRef); err != nil {
			return err
		}

		log.Info("Updating bootstrap reference",
			"api-version", machine.Spec.Bootstrap.ConfigRef.APIVersion,
			"kind", machine.Spec.Bootstrap.ConfigRef.Kind,
			"name", machine.Spec.Bootstrap.ConfigRef.Name,
		)
		if err := u.updateBootstrapConfig(replacementKey, machine.Spec.Bootstrap.ConfigRef.Name); err != nil {
			return err
		}

		log.Info("Updating machine")
		if err := u.updateMachine(replacementKey, machine); err != nil {
			return err
		}
	}

	return nil
}

func upgradeSuffix(upgradeID string) string {
	return ".upgrade." + upgradeID
}

// generateReplacementMachineName takes the original machine name and appends the upgrade suffix to it, removing any previous
// suffix. If the generated name would be longer than the maximum allowed name length, generateReplacementMachineName truncates
// the original name until the upgrade suffix fits.
func generateReplacementMachineName(original, upgradeID string) string {
	machineName := original
	match := upgradeIDNameSuffixRegex.FindStringIndex(machineName)
	machineSuffix := upgradeSuffix(upgradeID)
	if match != nil {
		index := match[0] - 1
		machineName = machineName[0:index]
	}

	excess := len(machineName) + len(machineSuffix) - validation.DNS1123SubdomainMaxLength
	if excess > 0 {
		max := len(machineName) - excess
		machineName = machineName[0:max]
	}

	return machineName + machineSuffix
}

func (u *ControlPlaneUpgrader) updateBootstrapConfig(replacementKey ctrlclient.ObjectKey, configName string) error {
	// Step 1: return early if we've already created the replacement infra resource
	replacementRef := v1.ObjectReference{
		APIVersion: bootstrapv1.GroupVersion.String(),
		Kind:       "KubeadmConfig",
		Namespace:  replacementKey.Namespace,
		Name:       replacementKey.Name,
	}
	exists, err := u.resourceExists(replacementRef)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	// Step 2: if we're here, we need to create it

	// copy node registration
	bootstrap := &bootstrapv1.KubeadmConfig{}
	bootstrapKey := ctrlclient.ObjectKey{
		Name:      configName,
		Namespace: u.clusterNamespace,
	}
	if err := u.managementClusterClient.Get(context.TODO(), bootstrapKey, bootstrap); err != nil {
		return errors.WithStack(err)
	}

	// modify bootstrap config
	bootstrap.SetName(replacementKey.Name)
	bootstrap.SetResourceVersion("")
	bootstrap.SetOwnerReferences(nil)

	// find node registration
	nodeRegistration := kubeadmv1beta1.NodeRegistrationOptions{}
	if bootstrap.Spec.InitConfiguration != nil {
		nodeRegistration = bootstrap.Spec.InitConfiguration.NodeRegistration
	} else if bootstrap.Spec.JoinConfiguration != nil {
		nodeRegistration = bootstrap.Spec.JoinConfiguration.NodeRegistration
	}
	if bootstrap.Spec.JoinConfiguration == nil {
		bootstrap.Spec.JoinConfiguration = &kubeadmv1beta1.JoinConfiguration{
			ControlPlane: &kubeadmv1beta1.JoinControlPlane{},
		}
	}
	bootstrap.Spec.JoinConfiguration.NodeRegistration = nodeRegistration

	// clear init configuration
	// When you have both the init configuration and the join configuration present
	// for a control plane upgrade, kubeadm will use the init configuration instead
	// of the join configuration. during upgrades, you will never be initializing a
	// new node. It will always be joining an existing control plane.
	bootstrap.Spec.InitConfiguration = nil

	err = u.managementClusterClient.Create(context.TODO(), bootstrap)
	if err != nil {
		return errors.WithStack(err)
	}

	// Return early if we've already updated the ownerRefs
	if u.secretsUpdated {
		return nil
	}

	secretNames := []string{
		fmt.Sprintf("%s-ca", u.clusterName),
		fmt.Sprintf("%s-etcd", u.clusterName),
		fmt.Sprintf("%s-sa", u.clusterName),
		fmt.Sprintf("%s-proxy", u.clusterName),
	}

	for _, secretName := range secretNames {
		secret := &v1.Secret{}
		secretKey := ctrlclient.ObjectKey{Name: secretName, Namespace: u.clusterNamespace}
		if err := u.managementClusterClient.Get(context.TODO(), secretKey, secret); err != nil {
			return errors.WithStack(err)
		}
		helper, err := patch.NewHelper(secret.DeepCopy(), u.managementClusterClient)
		if err != nil {
			return err
		}

		secret.SetOwnerReferences([]metav1.OwnerReference{
			metav1.OwnerReference{
				APIVersion: bootstrapv1.GroupVersion.String(),
				Kind:       "KubeadmConfig",
				Name:       bootstrap.Name,
				UID:        bootstrap.UID,
			},
		})

		if err := helper.Patch(context.TODO(), secret); err != nil {
			return err
		}
	}

	u.secretsUpdated = true

	return nil
}

func (u *ControlPlaneUpgrader) resourceExists(ref v1.ObjectReference) (bool, error) {
	obj := new(unstructured.Unstructured)
	obj.SetAPIVersion(ref.APIVersion)
	obj.SetKind(ref.Kind)
	key := ctrlclient.ObjectKey{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}
	if err := u.managementClusterClient.Get(context.TODO(), key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, errors.WithStack(err)
	}

	return true, nil
}

func (u *ControlPlaneUpgrader) updateInfrastructureReference(replacementKey ctrlclient.ObjectKey, ref v1.ObjectReference) error {
	// Step 1: return early if we've already created the replacement infra resource
	replacementRef := v1.ObjectReference{
		APIVersion: ref.APIVersion,
		Kind:       ref.Kind,
		Namespace:  replacementKey.Namespace,
		Name:       replacementKey.Name,
	}
	exists, err := u.resourceExists(replacementRef)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	// Step 2: if we're here, we need to create it

	// get original infrastructure object
	infraRef, err := external.Get(u.managementClusterClient, &ref, u.clusterNamespace)
	if err != nil {
		return err
	}

	// prep the replacement
	infraRef.SetResourceVersion("")
	infraRef.SetName(replacementKey.Name)
	infraRef.SetOwnerReferences(nil)
	unstructured.RemoveNestedField(infraRef.UnstructuredContent(), "spec", "providerID")

	// point the machine at the replacement

	// create the replacement infrastructure object
	err = u.managementClusterClient.Create(context.TODO(), infraRef)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func hostnameForNode(node *v1.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type == v1.NodeHostName {
			return address.Address
		}
	}
	return ""
}

func (u *ControlPlaneUpgrader) listMachines() ([]*clusterv1.Machine, error) {
	labels := ctrlclient.MatchingLabels{
		clusterv1.MachineClusterLabelName:      u.clusterName,
		clusterv1.MachineControlPlaneLabelName: "true",
	}
	listOptions := []ctrlclient.ListOption{
		labels,
		ctrlclient.InNamespace(u.clusterNamespace),
	}
	machines := &clusterv1.MachineList{}

	u.log.Info("Listing machines", "labelSelector", labels)
	err := u.managementClusterClient.List(context.TODO(), machines, listOptions...)
	if err != nil {
		return nil, errors.Wrap(err, "error listing machines")
	}

	var ret []*clusterv1.Machine
	for i := range machines.Items {
		m := machines.Items[i]
		if m.DeletionTimestamp.IsZero() {
			ret = append(ret, &m)
		}
	}

	return ret, nil
}

type etcdMembersResponse struct {
	Members []etcdMember `json:"members"`
}

type etcdMember struct {
	ID         uint64   `json:"ID"`
	Name       string   `json:"name"`
	ClientURLs []string `json:"clientURLs"`
}

func (u *ControlPlaneUpgrader) listEtcdMembers(timeout time.Duration) ([]etcdMember, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), timeout)
	defer cancel()

	stdout, _, err := u.etcdctl(ctx, "member list -w json")
	if err != nil {
		return []etcdMember{}, err
	}

	var resp etcdMembersResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		return []etcdMember{}, errors.Wrap(err, "unable to parse etcdctl member list json output")
	}

	return resp.Members, nil
}

func (u *ControlPlaneUpgrader) oldNodeToEtcdMemberId(timeout time.Duration) error {
	members, err := u.listEtcdMembers(timeout)
	if err != nil {
		return err
	}

	m := make(map[string]string)
	for _, member := range members {
		// etcd expects member IDs in hex, so convert to base 16
		id := strconv.FormatUint(member.ID, 16)
		m[member.Name] = id
	}

	u.oldNodeToEtcdMember = m

	return nil
}

// deleteEtcdMember deletes the old etcd member
func (u *ControlPlaneUpgrader) deleteEtcdMember(timeout time.Duration, etcdMemberId string) error {
	u.log.Info("Deleting etcd member", "id", etcdMemberId)
	ctx, cancel := context.WithTimeout(context.TODO(), timeout)
	defer cancel()

	_, _, err := u.etcdctl(ctx, "member", "remove", etcdMemberId)
	return err
}

func (u *ControlPlaneUpgrader) listEtcdPods() ([]v1.Pod, error) {
	// get pods in kube-system with label component=etcd
	list, err := u.targetKubernetesClient.CoreV1().Pods("kube-system").List(metav1.ListOptions{LabelSelector: "component=etcd"})
	if err != nil {
		return []v1.Pod{}, errors.Wrap(err, "error listing pods")
	}
	return list.Items, nil
}

func (u *ControlPlaneUpgrader) etcdctl(ctx context.Context, args ...string) (string, string, error) {
	pods, err := u.listEtcdPods()
	if err != nil {
		return "", "", err
	}
	if len(pods) == 0 {
		return "", "", errors.New("found 0 etcd pods")
	}

	var (
		stdout, stderr string
	)

	// Try all etcd pods. Return as soon as we get a successful result.
	for _, pod := range pods {
		stdout, stderr, err = u.etcdctlForPod(ctx, &pod, args...)
		if err == nil {
			return stdout, stderr, nil
		}
	}
	return stdout, stderr, err
}

func (u *ControlPlaneUpgrader) etcdctlForPod(ctx context.Context, pod *v1.Pod, args ...string) (string, string, error) {
	u.log.Info("Running etcdctl", "pod", pod.Name, "args", strings.Join(args, " "))

	endpoint := fmt.Sprintf("https://%s:2379", pod.Status.PodIP)

	fullArgs := []string{
		"ETCDCTL_API=3",
		"etcdctl",
		"--cacert", etcdCACertFile,
		"--cert", etcdCertFile,
		"--key", etcdKeyFile,
		"--endpoints", endpoint,
	}

	fullArgs = append(fullArgs, args...)

	opts := kubernetes2.PodExecInput{
		RestConfig:       u.targetRestConfig,
		KubernetesClient: u.targetKubernetesClient,
		Namespace:        pod.Namespace,
		Name:             pod.Name,
		Command: []string{
			"sh",
			"-c",
			strings.Join(fullArgs, " "),
		},
	}

	opts.Command = append(opts.Command, args...)

	stdout, stderr, err := kubernetes2.PodExec(ctx, opts)

	// TODO figure out how we want logs to show up in this library
	u.log.Info(fmt.Sprintf("etcdctl stdout: %s", stdout))
	u.log.Info(fmt.Sprintf("etcdctl stderr: %s", stderr))

	return stdout, stderr, err
}

// updateAndUploadKubeadmKubernetesVersion updates the Kubernetes version stored in the kubeadm configmap. This is
// required so that new Machines joining the cluster use the correct Kubernetes version as part of the upgrade.
func (u *ControlPlaneUpgrader) updateAndUploadKubeadmKubernetesVersion() error {
	original, err := u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Get("kubeadm-config", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "error getting kubeadm configmap from target cluster")
	}

	updated, err := updateKubeadmKubernetesVersion(original, "v"+u.desiredVersion.String())
	if err != nil {
		return err
	}

	if _, err = u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Update(updated); err != nil {
		return errors.Wrap(err, "error updating kubeadm configmap")
	}

	return nil
}

func updateKubeadmKubernetesVersion(original *v1.ConfigMap, version string) (*v1.ConfigMap, error) {
	cm := original.DeepCopy()

	clusterConfig := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(cm.Data["ClusterConfiguration"]), &clusterConfig); err != nil {
		return nil, errors.Wrap(err, "error decoding kubeadm configmap ClusterConfiguration")
	}

	clusterConfig["kubernetesVersion"] = version

	updated, err := yaml.Marshal(clusterConfig)
	if err != nil {
		return nil, errors.Wrap(err, "error encoding kubeadm configmap ClusterConfiguration")
	}

	cm.Data["ClusterConfiguration"] = string(updated)

	return cm, nil
}

func (u *ControlPlaneUpgrader) GetNodeFromProviderID(providerID string) *v1.Node {
	node, ok := u.providerIDsToNodes[providerID]
	if ok {
		return node
	}
	return nil
}

// UpdateProviderIDsToNodes retrieves a map that pairs a providerID to the node by listing all Nodes
// providerID : Node
func (u *ControlPlaneUpgrader) UpdateProviderIDsToNodes() error {
	u.log.Info("Updating provider IDs to nodes")
	nodes, err := u.targetKubernetesClient.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(err, "error listing nodes")
	}

	pairs := make(map[string]*v1.Node)
	for i := range nodes.Items {
		node := nodes.Items[i]
		id := ""
		providerID, err := noderefutil.NewProviderID(node.Spec.ProviderID)
		if err == nil {
			id = providerID.ID()
		} else {
			u.log.Error(err, "failed to parse provider id", "id", node.Spec.ProviderID, "node", node.Name)
			// unable to parse provider ID with whitelist of provider ID formats. Use original provider ID
			id = node.Spec.ProviderID
		}
		pairs[id] = &node
	}

	u.providerIDsToNodes = pairs

	return nil
}

func (u *ControlPlaneUpgrader) waitForProviderID(ns, name string, timeout time.Duration) (string, error) {
	log := u.log.WithValues("namespace", ns, "name", name)
	log.Info("Waiting for machine to have a provider id")
	var providerID string
	err := wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		machine := &clusterv1.Machine{}
		if err := u.managementClusterClient.Get(context.TODO(), ctrlclient.ObjectKey{Name: name, Namespace: ns}, machine); err != nil {
			log.Error(err, "Error getting machine, will try again")
			return false, nil
		}

		if machine.Spec.ProviderID == nil {
			return false, nil
		}

		providerID = *machine.Spec.ProviderID
		if providerID != "" {
			log.Info("Got provider id", "provider-id", providerID)
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		return "", errors.Wrap(err, "timed out waiting for machine provider id")
	}

	return providerID, nil
}

func (u *ControlPlaneUpgrader) waitForMatchingNode(rawProviderID string, timeout time.Duration) (*v1.Node, error) {
	u.log.Info("Waiting for node", "provider-id", rawProviderID)
	var matchingNode v1.Node
	providerID, err := noderefutil.NewProviderID(rawProviderID)
	if err != nil {
		return nil, err
	}

	err = wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		nodes, err := u.targetKubernetesClient.CoreV1().Nodes().List(metav1.ListOptions{})
		if err != nil {
			u.log.Error(err, "Error listing nodes in target cluster, will try again")
			return false, nil
		}
		for _, node := range nodes.Items {
			nodeID, err := noderefutil.NewProviderID(node.Spec.ProviderID)
			if err != nil {
				u.log.Error(err, "unable to process node's provider ID", "node", node.Name, "provider-id", node.Spec.ProviderID)
				// Continue instead of returning so we can process all the nodes in the list
				continue
			}
			if providerID.Equals(nodeID) {
				u.log.Info("Found node", "name", node.Name)
				matchingNode = node
				return true, nil
			}
		}

		return false, nil
	})

	if err != nil {
		return nil, errors.Wrap(err, "timed out waiting for matching node")
	}

	return &matchingNode, nil
}

func (u *ControlPlaneUpgrader) waitForNodeReady(newNode *v1.Node, timeout time.Duration) error {
	// wait for NodeReady
	nodeHostname := hostnameForNode(newNode)
	if nodeHostname == "" {
		u.log.Info("unable to find hostname for node", "node", newNode.Name)
		return errors.Errorf("unable to find hostname for node %s", newNode.Name)
	}
	err := wait.PollImmediate(15*time.Second, timeout, func() (bool, error) {
		ready := u.isReady(nodeHostname)
		return ready, nil
	})
	if err != nil {
		return errors.Wrapf(err, "components on node %s are not ready", newNode.Name)
	}
	return nil
}

func (u *ControlPlaneUpgrader) isReady(nodeHostname string) bool {
	u.log.Info("Component health check for node", "hostname", nodeHostname)

	components := []string{"etcd", "kube-apiserver", "kube-scheduler", "kube-controller-manager"}
	requiredConditions := sets.NewString("PodScheduled", "Initialized", "Ready", "ContainersReady")

	for _, component := range components {
		foundConditions := sets.NewString()

		podName := fmt.Sprintf("%s-%v", component, nodeHostname)
		log := u.log.WithValues("pod", podName)

		log.Info("Getting pod")
		pod, err := u.targetKubernetesClient.CoreV1().Pods("kube-system").Get(podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			log.Info("Pod not found yet")
			return false
		} else if err != nil {
			log.Error(err, "error getting pod")
			return false
		}

		for _, condition := range pod.Status.Conditions {
			if condition.Status == "True" {
				foundConditions.Insert(string(condition.Type))
			}
		}

		missingConditions := requiredConditions.Difference(foundConditions)
		if missingConditions.Len() > 0 {
			missingDescription := strings.Join(missingConditions.List(), ",")
			log.Info("pod is missing some required conditions", "conditions", missingDescription)
			return false
		}
	}

	return true
}
