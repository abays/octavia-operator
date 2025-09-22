/*

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

// Package amphoracontrollers provides controllers for managing Octavia amphora instances and resources
package amphoracontrollers

import (
	"fmt"
	"sort"

	topologyv1 "github.com/openstack-k8s-operators/infra-operator/apis/topology/v1beta1"
	"github.com/openstack-k8s-operators/lib-common/modules/common"
	"github.com/openstack-k8s-operators/lib-common/modules/common/affinity"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	octaviav1 "github.com/openstack-k8s-operators/octavia-operator/api/v1beta1"
	"github.com/openstack-k8s-operators/octavia-operator/pkg/octavia"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	// InitContainerCommand -
	InitContainerCommand = "/usr/local/bin/container-scripts/init.sh %s"
)

// DaemonSet func
func DaemonSet(
	instance *octaviav1.OctaviaAmphoraController,
	configHash string,
	labels map[string]string,
	annotations map[string]string,
	topology *topologyv1.Topology,
) *appsv1.DaemonSet {
	serviceName := fmt.Sprintf("octavia-%s", instance.Spec.Role)

	// The API pod has an extra volume so the API and the provider agent can
	// communicate with each other.
	volumes := GetVolumes(instance.Name)
	parentOctaviaName := octavia.GetOwningOctaviaControllerName(instance)
	certsSecretName := fmt.Sprintf("%s-certs-secret", parentOctaviaName)
	volumes = append(volumes, GetCertVolume(certsSecretName)...)

	volumeMounts := octavia.GetVolumeMounts(serviceName)
	volumeMounts = append(volumeMounts, GetCertVolumeMount()...)

	livenessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      15,
		PeriodSeconds:       13,
		InitialDelaySeconds: 3,
	}
	readinessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      15,
		PeriodSeconds:       15,
		InitialDelaySeconds: 5,
	}

	// TODO(beagles): use equivalent's of healthcheck's in tripleo which
	// seem to largely based on connections to database. The pgrep's
	// could be tightened up too but they seem to be a bit tricky.

	livenessProbe.Exec = &corev1.ExecAction{
		Command: []string{
			"/usr/bin/pgrep", "-r", "DRST", "octavia",
		},
	}

	readinessProbe.Exec = &corev1.ExecAction{
		Command: []string{
			"/usr/bin/pgrep", "-r", "DRST", "octavia",
		},
	}

	envVars := map[string]env.Setter{}

	envVars["KOLLA_CONFIG_STRATEGY"] = env.SetValue("COPY_ALWAYS")
	envVars["CONFIG_HASH"] = env.SetValue(configHash)
	envVars["NODE_NAME"] = env.DownwardAPI("spec.nodeName")

	if instance.Spec.OctaviaProviderSubnetCIDR != "" {
		envVars["MGMT_CIDR"] = env.SetValue(instance.Spec.OctaviaProviderSubnetCIDR)
	}
	envVars["MGMT_GATEWAY"] = env.SetValue(instance.Spec.OctaviaProviderSubnetGateway)

	if len(instance.Spec.OctaviaProviderSubnetExtraCIDRs) > 0 {
		// Sort the array to make it stable across calls to reconcile
		var extraCIDRs = make([]string, len(instance.Spec.OctaviaProviderSubnetExtraCIDRs))
		copy(extraCIDRs, instance.Spec.OctaviaProviderSubnetExtraCIDRs)
		sort.Strings(extraCIDRs)
		for idx, subnetCIDR := range extraCIDRs {
			envVars[fmt.Sprintf("MGMT_CIDR%d", idx)] = env.SetValue(subnetCIDR)
		}
	}

	// Add the CA bundle
	if instance.Spec.TLS.CaBundleSecretName != "" {
		volumes = append(volumes, instance.Spec.TLS.CreateVolume())
		volumeMounts = append(volumeMounts, instance.Spec.TLS.CreateVolumeMounts(nil)...)
	}

	args := []string{
		"-c",
		fmt.Sprintf(InitContainerCommand, instance.Name),
	}

	// When we don't use jobboard, we need to ensure that the octavia
	// controllers are gracefully shutdown, so after they receive the signal,
	// they need to complete the jobs that are being executed (creating a LB,
	// updating a listener)
	// 600 sec is close to the value that was used in tripleo, it's based on the
	// max duration of a flow in a worst case scenario (updating an amphora that
	// is not reachable).
	// The octavia [DEFAULT].graceful_shutdown_timeout is set accordingly
	var terminationGracePeriodSeconds int64
	if len(instance.Spec.RedisHosts) > 0 {
		terminationGracePeriodSeconds = int64(30)
	} else {
		terminationGracePeriodSeconds = int64(600)
	}

	capabilities := []corev1.Capability{"NET_ADMIN", "SYS_ADMIN", "SYS_NICE"}
	if instance.Spec.Role == octaviav1.HealthManager {
		// NET_RAW is required for IP advertisements
		capabilities = append(capabilities, corev1.Capability("NET_RAW"))
	}

	daemonset := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: instance.Namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: annotations,
					Labels:      labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: ptr.To(octavia.OctaviaUID),
					},
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
					ServiceAccountName:            instance.Spec.ServiceAccount,
					AutomountServiceAccountToken:  ptr.To(false),
					Containers: []corev1.Container{
						{
							Name:            serviceName,
							Image:           instance.Spec.ContainerImage,
							SecurityContext: octavia.GetOctaviaSecurityContext(),
							Env:             env.MergeEnvs([]corev1.EnvVar{}, envVars),
							VolumeMounts:    volumeMounts,
							Resources:       instance.Spec.Resources,
							ReadinessProbe:  readinessProbe,
							LivenessProbe:   livenessProbe,
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:  "init",
							Image: instance.Spec.ContainerImage,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add:  capabilities,
									Drop: []corev1.Capability{},
								},
								RunAsUser: ptr.To(int64(0)),
							},
							Command: []string{
								"/bin/bash",
							},
							Env:          env.MergeEnvs([]corev1.EnvVar{}, envVars),
							Args:         args,
							VolumeMounts: GetInitVolumeMounts(),
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
	// DaemonSet automatically place one Pod on every node that matches the
	// node selector, but topology spread constraints and affinity/antiaffinity
	// rules are ignored. Keeping the code as it is to make sure we do not
	// modify the .Spec on existing deployments (triggering a rollout/restart)
	// of the existing instances.
	// More details about DaemonSetSpec Pod scheduling in:
	// https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/daemon/daemon_controller.go#L1018
	if topology != nil {
		topology.ApplyTo(&daemonset.Spec.Template)
	} else {
		// If possible two pods of the same service should not
		// run on the same worker node. If this is not possible
		// the get still created on the same worker node.
		daemonset.Spec.Template.Spec.Affinity = affinity.DistributePods(
			common.AppSelector,
			[]string{
				serviceName,
			},
			corev1.LabelHostname,
		)
		if instance.Spec.NodeSelector != nil {
			daemonset.Spec.Template.Spec.NodeSelector = *instance.Spec.NodeSelector
		}
	}

	return daemonset
}
