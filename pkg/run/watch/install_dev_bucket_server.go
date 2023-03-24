package watch

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"strconv"
	"time"

	"github.com/weaveworks/weave-gitops/pkg/logger"
	"github.com/weaveworks/weave-gitops/pkg/run"
	"github.com/weaveworks/weave-gitops/pkg/run/constants"
	"github.com/weaveworks/weave-gitops/pkg/tls"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// The variables below are to be set by flags passed to `go build`.
	// Examples: -X run.DevBucketContainerImage=xxxxx

	DevBucketContainerImage string
)

// InstallDevBucketServer installs the dev bucket server, open port forwarding, and returns a function that can be used to the port forwarding.
func InstallDevBucketServer(
	ctx context.Context,
	log logger.Logger,
	kubeClient client.Client,
	config *rest.Config,
	httpPort,
	httpsPort int32,
	accessKey,
	secretKey []byte) (func(), []byte, error) {
	var (
		err                error
		devBucketAppLabels = map[string]string{
			"app": constants.RunDevBucketName,
		}
	)

	// create namespace
	devBucketNamespace := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: constants.GitOpsRunNamespace,
		},
	}

	log.Actionf("Checking namespace %s ...", constants.GitOpsRunNamespace)

	err = kubeClient.Get(ctx,
		client.ObjectKeyFromObject(&devBucketNamespace),
		&devBucketNamespace)

	if err != nil && apierrors.IsNotFound(err) {
		if err := kubeClient.Create(ctx, &devBucketNamespace); err != nil {
			log.Failuref("Error creating namespace %s: %v", constants.GitOpsRunNamespace, err.Error())
			return nil, nil, err
		} else {
			log.Successf("Created namespace %s", constants.GitOpsRunNamespace)
		}
	} else if err == nil {
		log.Successf("Namespace %s already existed", constants.GitOpsRunNamespace)
	}

	// create service
	devBucketService := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.RunDevBucketName,
			Namespace: constants.GitOpsRunNamespace,
			Labels:    devBucketAppLabels,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name: fmt.Sprintf("%s-http", constants.RunDevBucketName),
					Port: httpPort,
				},
				{
					Name: fmt.Sprintf("%s-https", constants.RunDevBucketName),
					Port: httpsPort,
				},
			},
			Selector: devBucketAppLabels,
		},
	}

	log.Actionf("Checking service %s/%s ...", constants.GitOpsRunNamespace, constants.RunDevBucketName)

	err = kubeClient.Get(ctx,
		client.ObjectKeyFromObject(&devBucketService),
		&devBucketService)

	if err != nil && apierrors.IsNotFound(err) {
		if err := kubeClient.Create(ctx, &devBucketService); err != nil {
			log.Failuref("Error creating service %s/%s: %v", constants.GitOpsRunNamespace, constants.RunDevBucketName, err.Error())
			return nil, nil, err
		} else {
			log.Successf("Created service %s/%s", constants.GitOpsRunNamespace, constants.RunDevBucketName)
		}
	} else if err == nil {
		log.Successf("Service %s/%s already existed", constants.GitOpsRunNamespace, constants.RunDevBucketName)
	}

	credentialsSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: constants.GitOpsRunNamespace,
			Name:      constants.RunDevBucketCredentials,
		},
		Data: map[string][]byte{
			"accesskey": accessKey,
			"secretkey": secretKey,
		},
	}
	if err := kubeClient.Create(ctx, &credentialsSecret); err != nil {
		log.Failuref("Error creating credentials secret: %s", err.Error())
		return nil, nil, fmt.Errorf("failed creating credentials secret: %w", err)
	}

	cert, err := tls.GenerateSelfSignedCertificate("localhost", fmt.Sprintf("%s.%s.svc.cluster.local", devBucketService.Name, devBucketService.Namespace))
	if err != nil {
		err = fmt.Errorf("failed generating self-signed certificate for dev bucket server: %w", err)
		log.Failuref(err.Error())

		return nil, nil, err
	}

	certsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-bucket-server-certs",
			Namespace: constants.GitOpsRunNamespace,
			Labels:    devBucketAppLabels,
		},
		Data: map[string][]byte{
			"cert.pem": cert.Cert,
			"cert.key": cert.Key,
		},
	}
	if err := kubeClient.Create(ctx, certsSecret); err != nil {
		log.Failuref("Error creating Secret %s/%s: %v", certsSecret.Namespace, certsSecret.Name, err.Error())
		return nil, nil, err
	}

	// create deployment
	replicas := int32(1)
	devBucketDeployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.RunDevBucketName,
			Namespace: constants.GitOpsRunNamespace,
			Labels:    devBucketAppLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: devBucketAppLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: devBucketAppLabels,
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name: "certs",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: "dev-bucket-server-certs",
							},
						},
					}},
					Containers: []corev1.Container{
						{
							Name:            constants.RunDevBucketName,
							Image:           DevBucketContainerImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{Name: "MINIO_ROOT_USER", ValueFrom: &corev1.EnvVarSource{
									SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: credentialsSecret.Name},
										Key:                  "accesskey",
									},
								}},
								{Name: "MINIO_ROOT_PASSWORD", ValueFrom: &corev1.EnvVarSource{
									SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: credentialsSecret.Name},
										Key:                  "secretkey",
									},
								}},
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: httpPort,
									HostPort:      httpPort,
								},
								{
									ContainerPort: httpsPort,
									HostPort:      httpsPort,
								},
							},
							Args: []string{
								fmt.Sprintf("--http-port=%d", httpPort),
								fmt.Sprintf("--https-port=%d", httpsPort),
								"--cert-file=/tmp/certs/cert.pem",
								"--key-file=/tmp/certs/cert.key",
							},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "certs",
								MountPath: "/tmp/certs",
							}},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}

	log.Actionf("Checking deployment %s/%s ...", constants.GitOpsRunNamespace, constants.RunDevBucketName)

	err = kubeClient.Get(ctx,
		client.ObjectKeyFromObject(&devBucketDeployment),
		&devBucketDeployment)

	if err != nil && apierrors.IsNotFound(err) {
		if err := kubeClient.Create(ctx, &devBucketDeployment); err != nil {
			log.Failuref("Error creating deployment %s/%s: %v", constants.GitOpsRunNamespace, constants.RunDevBucketName, err.Error())
			return nil, nil, err
		} else {
			log.Successf("Created deployment %s/%s", constants.GitOpsRunNamespace, constants.RunDevBucketName)
		}
	} else if err == nil {
		log.Successf("Deployment %s/%s already existed", constants.GitOpsRunNamespace, constants.RunDevBucketName)
	}

	log.Actionf("Waiting for deployment %s to be ready ...", constants.RunDevBucketName)

	if err := wait.ExponentialBackoff(wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   2,
		Jitter:   1,
		Steps:    10,
	}, func() (done bool, err error) {
		d := devBucketDeployment.DeepCopy()
		if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
			return false, err
		}
		// Confirm the state we are observing is for the current generation
		if d.Generation != d.Status.ObservedGeneration {
			return false, nil
		}

		if d.Status.ReadyReplicas == 1 {
			return true, nil
		}

		return false, nil
	}); err != nil {
		log.Failuref("Max retry exceeded waiting for deployment to be ready")
	}

	specMap := &PortForwardSpec{
		Name:          constants.RunDevBucketName,
		Namespace:     constants.GitOpsRunNamespace,
		Kind:          "service",
		HostPort:      strconv.Itoa(int(httpsPort)),
		ContainerPort: strconv.Itoa(int(httpsPort)),
	}
	// get pod from specMap
	namespacedName := types.NamespacedName{Namespace: specMap.Namespace, Name: specMap.Name}

	pod, err := run.GetPodFromResourceDescription(ctx, kubeClient, namespacedName, specMap.Kind, nil)
	if err != nil {
		log.Failuref("Error getting pod from specMap: %v", err)
	}

	if pod != nil {
		waitFwd := make(chan struct{}, 1)
		readyChannel := make(chan struct{})
		cancelPortFwd := func() {
			close(waitFwd)
		}

		log.Actionf("Port forwarding to pod %s/%s ...", pod.Namespace, pod.Name)

		go func() {
			if err := ForwardPort(log.L(), pod, config, specMap, waitFwd, readyChannel); err != nil {
				log.Failuref("Error forwarding port: %v", err)
			}
		}()
		<-readyChannel

		log.Successf("Port forwarding for %s is ready.", constants.RunDevBucketName)

		return cancelPortFwd, cert.Cert, nil
	}

	return nil, nil, fmt.Errorf("pod not found")
}

type resourceToDelete struct {
	key types.NamespacedName
	gvk schema.GroupVersionKind
}

func devBucketCleanUpFunc(ctx context.Context, log logger.Logger, kubeClient client.Client) ([]resourceToDelete, error) {
	// Rsources to delete:
	// Service: constants.RunDevBucketName in the namespace constants.GitOpsRunNamespace
	// Deployment: constants.RunDevBucketName in the namespace constants.GitOpsRunNamespace
	// Secret: dev-bucket-server-certs in the namespace constants.GitOpsRunNamespace
	// Secret: constants.RunDevBucketCredentials in the namespace constants.GitOpsRunNamespace
	// Namespace: constants.GitOpsRunNamespace

	var allResources []resourceToDelete

	// delete deployment
	devBucketDeployment := appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.RunDevBucketName,
			Namespace: constants.GitOpsRunNamespace,
		},
	}
	log.Actionf("Deleting deployment %s/%s ...", constants.GitOpsRunNamespace, constants.RunDevBucketName)

	if err := kubeClient.Delete(ctx, &devBucketDeployment); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Failuref("Error deleting deployment %s/%s: %v", constants.GitOpsRunNamespace, constants.RunDevBucketName, err.Error())
			return nil, err
		}
	}
	allResources = append(allResources, resourceToDelete{
		key: client.ObjectKeyFromObject(&devBucketDeployment),
		gvk: devBucketDeployment.GroupVersionKind(),
	})

	// delete service
	devBucketService := corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.RunDevBucketName,
			Namespace: constants.GitOpsRunNamespace,
		},
	}
	log.Actionf("Deleting service %s/%s ...", constants.GitOpsRunNamespace, constants.RunDevBucketName)

	if err := kubeClient.Delete(ctx, &devBucketService); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Failuref("Error deleting service %s/%s: %v", constants.GitOpsRunNamespace, constants.RunDevBucketName, err.Error())
			return nil, err
		}
	}
	allResources = append(allResources, resourceToDelete{
		key: client.ObjectKeyFromObject(&devBucketService),
		gvk: devBucketService.GroupVersionKind(),
	})

	// delete secret
	devBucketSecret := corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.RunDevBucketCredentials,
			Namespace: constants.GitOpsRunNamespace,
		},
	}
	log.Actionf("Deleting secret %s/%s ...", constants.GitOpsRunNamespace, constants.RunDevBucketCredentials)

	if err := kubeClient.Delete(ctx, &devBucketSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Failuref("Error deleting secret %s/%s: %v", constants.GitOpsRunNamespace, constants.RunDevBucketCredentials, err.Error())
			return nil, err
		}
	}
	allResources = append(allResources, resourceToDelete{
		key: client.ObjectKeyFromObject(&devBucketSecret),
		gvk: devBucketSecret.GroupVersionKind(),
	})

	// delete secret
	devBucketServerCerts := corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-bucket-server-certs",
			Namespace: constants.GitOpsRunNamespace,
		},
	}
	log.Actionf("Deleting secret %s/%s ...", constants.GitOpsRunNamespace, "dev-bucket-server-certs")

	if err := kubeClient.Delete(ctx, &devBucketServerCerts); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Failuref("Error deleting secret %s/%s: %v", constants.GitOpsRunNamespace, "dev-bucket-server-certs", err.Error())
			return nil, err
		}
	}
	allResources = append(allResources, resourceToDelete{
		key: client.ObjectKeyFromObject(&devBucketServerCerts),
		gvk: devBucketServerCerts.GroupVersionKind(),
	})

	// delete namespace
	devBucketNamespace := corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Namespace",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: constants.GitOpsRunNamespace,
		},
	}
	log.Actionf("Deleting namespace %s ...", constants.GitOpsRunNamespace)

	if err := kubeClient.Delete(ctx, &devBucketNamespace); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Failuref("Error deleting namespace %s: %v", constants.GitOpsRunNamespace, err.Error())
			return nil, err
		}
	}
	allResources = append(allResources, resourceToDelete{
		key: client.ObjectKeyFromObject(&devBucketNamespace),
		gvk: devBucketNamespace.GroupVersionKind(),
	})

	return allResources, nil
}

// UninstallDevBucketServer deletes the dev-bucket namespace.
func UninstallDevBucketServer(ctx context.Context, log logger.Logger, kubeClient client.Client) error {
	resources, err := devBucketCleanUpFunc(ctx, log, kubeClient)
	if err != nil {
		return err
	}

	log.Actionf("Waiting for resources to be terminated ...")

	// The purpose of this code is to wait for a list of Kubernetes resources to be deleted from a namespace,
	// using an exponential backoff strategy to avoid overloading the Kubernetes API server with requests.
	//
	// The wait.ExponentialBackoff function is called with a wait.Backoff struct, defining the exponential backoff settings.
	// The function provided as the second argument to wait.ExponentialBackoff checks the status of the Kubernetes resources in the resources slice.
	// For each resource in the resources slice, the code attempts to retrieve the resource using kubeClient.Get.
	// - If the resource is not found (i.e., apierrors.IsNotFound(err) returns true), the loop continues checking the next resource.
	// - If the resource is found (i.e., there is no error), the function returns false, nil, indicating that the operation is not yet done.
	//   The wait.ExponentialBackoff function will retry the operation based on the backoff settings.
	// - If an error other than "not found" occurs, the function returns false, err.
	//   The wait.ExponentialBackoff function stops retrying immediately and returns the error.
	// - If all resources are checked and not found, the function returns true, nil,
	//   indicating that the operation is done, and no errors occurred.
	// - If the maximum number of retries (backoff.Steps) is reached or the backoff duration is capped,
	//   and the resources are not yet deleted, the wait.ExponentialBackoff function returns ErrWaitTimeout.
	//   In this case, the log message "Max retry exceeded waiting for resources to be deleted" will be printed.
	if err := wait.ExponentialBackoff(wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   2,
		Jitter:   1,
		Steps:    10,
	}, func() (done bool, err error) {
		for _, resource := range resources {
			u := &unstructured.Unstructured{}
			// u.SetGroupVersionKind(resource.gvk)
			u.SetKind(resource.gvk.Kind)
			u.SetAPIVersion(resource.gvk.GroupVersion().String())
			if err := kubeClient.Get(ctx, resource.key, u); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				} else {
					return false, err
				}
			}
			return false, nil
		}
		return true, nil
	}); err != nil {
		log.Failuref("Max retry exceeded waiting for resources to be deleted: %v", err.Error())
	}

	log.Successf("Resources terminated")

	return nil
}
