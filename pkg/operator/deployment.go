package operator

import (
	// standard lib
	"fmt"
	// 3rd party
	"github.com/sirupsen/logrus"
	// kubernetes
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	// openshift
	"github.com/operator-framework/operator-sdk/pkg/sdk"
	// operator
	"github.com/openshift/console-operator/pkg/apis/console/v1alpha1"
)

const (
	configMapResourceVersionAnnotation = "console.openshift.io/configmapversion"
)

func newConsoleDeployment(cr *v1alpha1.Console) *appsv1.Deployment {
	labels := labelsForConsole()
	meta := sharedMeta()
	replicas := cr.Spec.Count
	// tack on the deployment specific labels
	// TODO: just make this another helper function, ensure things stay in sync
	meta.Labels = labels
	gracePeriod := int64(30)

	deployment := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: meta,
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:   OpenShiftConsoleShortName,
					Labels: labels,
					Annotations: map[string]string{
						configMapResourceVersionAnnotation: "",
					},
				},
				Spec: corev1.PodSpec{
					// NodeSelector:  corev1.NodeSelector{},
					RestartPolicy: "Always",
					SchedulerName: "default-scheduler",
					//the values here may be openshift specific.
					//Affinity: corev1.Affinity{ },
					TerminationGracePeriodSeconds: &gracePeriod,
					SecurityContext:               &corev1.PodSecurityContext{},
					Containers: []corev1.Container{
						consoleContainer(cr),
					},
					Volumes: consoleVolumes(volumeConfigList),
				},
			},
		},
	}
	addOwnerRef(deployment, ownerRefFrom(cr))
	return deployment
}

// deduplication, use the same volume config to generate Volumes, and VolumeMounts
func consoleVolumes(vc []volumeConfig) []corev1.Volume {
	vols := make([]corev1.Volume, len(vc))
	for i, item := range vc {
		if item.isSecret {
			vols[i] = corev1.Volume{
				Name: item.name,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: item.name,
					},
				},
			}
		}
		if item.isConfigMap {
			vols[i] = corev1.Volume{
				Name: item.name,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: item.name,
						},
					},
				},
			}
		}
	}
	return vols
}

func consoleVolumeMounts(vc []volumeConfig) []corev1.VolumeMount {
	volMountList := make([]corev1.VolumeMount, len(vc))
	for i, item := range vc {
		volMountList[i] = corev1.VolumeMount{
			Name:      item.name,
			ReadOnly:  item.readOnly,
			MountPath: item.path,
		}
	}
	return volMountList
}

func image(base string, version string) string {
	if version != "" {
		return fmt.Sprintf("%s:%s", base, version)
	}
	return fmt.Sprintf("%s", base)
}

func defaultProbe() *corev1.Probe {
	return &corev1.Probe{
		Handler: corev1.Handler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/health",
				Port:   intstr.FromInt(8443),
				Scheme: corev1.URIScheme("HTTPS"),
			},
		},
		TimeoutSeconds:   1,
		PeriodSeconds:    10,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
}

func livenessProbe() *corev1.Probe {
	probe := defaultProbe()
	probe.InitialDelaySeconds = 30
	return probe
}

func consoleContainer(cr *v1alpha1.Console) corev1.Container {
	volumeMounts := consoleVolumeMounts(volumeConfigList)

	return corev1.Container{
		Image:           GetImageEnv(),
		ImagePullPolicy: corev1.PullPolicy("IfNotPresent"),
		Name:            OpenShiftConsoleShortName,
		Command: []string{
			"/opt/bridge/bin/bridge",
			"--public-dir=/opt/bridge/static",
			"--config=/var/console-config/console-config.yaml",
		},
		// TODO: can probably remove, this is used for local dev
		//Env: []corev1.EnvVar{{
		//	Name:  publicURLName,
		//	Value: consoleURL(),
		//}},
		Ports: []corev1.ContainerPort{{
			Name:          consolePortName,
			Protocol:      corev1.ProtocolTCP,
			ContainerPort: consolePort,
		}},
		VolumeMounts:             volumeMounts,
		ReadinessProbe:           defaultProbe(),
		LivenessProbe:            livenessProbe(),
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessagePolicy("File"),
		Resources: corev1.ResourceRequirements{
			Limits: map[corev1.ResourceName]resource.Quantity{
				// TODO: fill these out
				//	"cpu": int64(100),
				//	"memory": int64(100)
			},
			Requests: map[corev1.ResourceName]resource.Quantity{},
		},
	}

}

func UpdateDeployment(cr *v1alpha1.Console, cm *corev1.ConfigMap) (*appsv1.Deployment, error) {
	d := newConsoleDeployment(cr)
	changed := false
	if err := sdk.Get(d); err != nil {
		return nil, err
	}

	if d.Spec.Template.ObjectMeta.Annotations == nil {
		d.Spec.Template.ObjectMeta.Annotations = map[string]string{}
		changed = true
	}

	currentConfigMapVersion := d.Spec.Template.ObjectMeta.Annotations[configMapResourceVersionAnnotation]

	if currentConfigMapVersion != cm.ObjectMeta.GetResourceVersion() {
		// If the configmap is updated, we want to rollout a new pod.
		// setting an annotation is sufficient for this task.
		d.Spec.Template.ObjectMeta.Annotations[configMapResourceVersionAnnotation] = cm.ObjectMeta.GetResourceVersion()
		changed = true
	}

	if changed {
		if err := sdk.Update(d); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func CreateConsoleDeployment(cr *v1alpha1.Console) (*appsv1.Deployment, error) {
	d := newConsoleDeployment(cr)
	if err := sdk.Create(d); err != nil && !errors.IsAlreadyExists(err) {
		logrus.Errorf("failed to create console deployment : %v", err)
		return nil, err
	}
	logrus.Infof("created console deployment '%s'", d.ObjectMeta.Name)
	return d, nil
}

func ApplyDeployment(cr *v1alpha1.Console, cm *corev1.ConfigMap) (*appsv1.Deployment, error) {
	d := newConsoleDeployment(cr)
	err := sdk.Get(d)

	if err != nil {
		return CreateConsoleDeployment(cr)
	}
	return UpdateDeployment(cr, cm)
}

// Deletes the Console Deployment when the Console ManagementState is set to Removed
func DeleteDeployment(cr *v1alpha1.Console) error {
	d := newConsoleDeployment(cr)
	return sdk.Delete(d)
}
