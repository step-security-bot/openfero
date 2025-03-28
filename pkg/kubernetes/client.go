package kubernetes

import (
	"context"
	"os"
	"time"

	log "github.com/OpenFero/openfero/pkg/logging"
	"github.com/OpenFero/openfero/pkg/metadata"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// Client represents a Kubernetes client with necessary stores and configuration
type Client struct {
	Clientset               kubernetes.Clientset
	JobDestinationNamespace string
	ConfigmapNamespace      string
	ConfigMapStore          cache.Store
	JobStore                cache.Store
	LabelSelector           *metav1.LabelSelector
}

// InitKubeClient initializes a Kubernetes client using in-cluster or kubeconfig
func InitKubeClient(kubeconfig *string) *kubernetes.Clientset {
	var config *rest.Config
	var err error

	// Try in-cluster config first
	config, err = rest.InClusterConfig()
	if err != nil {
		log.Debug("In-cluster configuration not available, trying kubeconfig file")
		// Use kubeconfig file
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			log.Fatal("Could not create k8s configuration", zap.String("error", err.Error()))
		}
		log.Info("Using kubeconfig file for cluster access")
	} else {
		log.Info("Using in-cluster configuration")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("Could not create k8s client", zap.String("error", err.Error()))
	}

	return clientset
}

// GetCurrentNamespace determines the current namespace
func GetCurrentNamespace() (string, error) {
	// Check if running in-cluster
	_, err := rest.InClusterConfig()

	if err != nil {
		// Out-of-cluster
		log.Debug("Using out of cluster configuration")
		// Extract namespace from client config
		namespace, _, err := clientcmd.DefaultClientConfig.Namespace()
		return namespace, err
	} else {
		// In-cluster
		log.Debug("Using in cluster configuration")
		// Read namespace from mounted secrets
		defaultNamespaceLocation := "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
		if _, statErr := os.Stat(defaultNamespaceLocation); os.IsNotExist(statErr) {
			return "", statErr
		}
		namespaceDat, readErr := os.ReadFile(defaultNamespaceLocation)
		if readErr != nil {
			return "", readErr
		}
		return string(namespaceDat), nil
	}
}

// InitConfigMapInformer initializes a ConfigMap informer
func InitConfigMapInformer(clientset *kubernetes.Clientset, configmapNamespace string, labelSelector *metav1.LabelSelector) cache.Store {
	// Create informer factory
	configMapfactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		time.Hour*1,
		informers.WithNamespace(configmapNamespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = metav1.FormatLabelSelector(labelSelector)
		}),
	)

	// Get ConfigMap informer
	configMapInformer := configMapfactory.Core().V1().ConfigMaps().Informer()

	// Add event handlers
	if _, err := configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			log.Debug("ConfigMap added to store")
		},
		UpdateFunc: func(old, new interface{}) {
			log.Debug("ConfigMap updated in store")
		},
		DeleteFunc: func(obj interface{}) {
			log.Debug("ConfigMap removed from store")
		},
	}); err != nil {
		log.Fatal("Failed to add ConfigMap event handler", zap.String("error", err.Error()))
	}

	// Start informer
	go configMapfactory.Start(context.Background().Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(context.Background().Done(), configMapInformer.HasSynced) {
		log.Fatal("Failed to sync ConfigMap cache")
	}

	return configMapInformer.GetStore()
}

// InitJobInformer initializes a Job informer
func InitJobInformer(clientset *kubernetes.Clientset, jobDestinationNamespace string, labelSelector *metav1.LabelSelector) cache.Store {
	// Create informer factory
	jobFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		time.Hour*1,
		informers.WithNamespace(jobDestinationNamespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = metav1.FormatLabelSelector(labelSelector)
		}),
	)

	// Get Job informer
	jobInformer := jobFactory.Batch().V1().Jobs().Informer()

	// Add event handlers
	if _, err := jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			job := obj.(*batchv1.Job)
			log.Debug("Job added: " + job.Name)
			metadata.JobsCreatedTotal.Inc()
		},
		UpdateFunc: func(old, new interface{}) {
			oldJob := old.(*batchv1.Job)
			newJob := new.(*batchv1.Job)
			if newJob.Status.Succeeded > 0 && oldJob.Status.Succeeded == 0 {
				log.Debug("Job completed successfully: " + newJob.Name)
				metadata.JobsSucceededTotal.Inc()
			}
			if newJob.Status.Failed > 0 && oldJob.Status.Failed == 0 {
				log.Debug("Job failed: " + newJob.Name)
				metadata.JobsFailedTotal.Inc()
			}
		},
		DeleteFunc: func(obj interface{}) {
			job := obj.(*batchv1.Job)
			log.Debug("Job deleted: " + job.Name)
		},
	}); err != nil {
		log.Fatal("Failed to add Job event handler", zap.String("error", err.Error()))
	}

	// Start informer
	go jobFactory.Start(context.Background().Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(context.Background().Done(), jobInformer.HasSynced) {
		log.Fatal("Failed to sync Job cache")
	}

	return jobInformer.GetStore()
}
