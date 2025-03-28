package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/OpenFero/openfero/pkg/logging"
	"github.com/OpenFero/openfero/pkg/models"
	"github.com/OpenFero/openfero/pkg/utils"
	"github.com/ghodss/yaml"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CreateRemediationJob creates a new job in the specified namespace
func (c *Client) CreateRemediationJob(jobObject *batchv1.Job) error {
	// Check if job already exists
	_, exists, err := c.JobStore.GetByKey(c.JobDestinationNamespace + "/" + jobObject.Name)
	if err != nil {
		log.Error("error checking job existence: ", zap.String("error", err.Error()))
		return err
	}
	if exists {
		return fmt.Errorf("job %s already exists", jobObject.Name)
	}

	// Create job
	jobsClient := c.Clientset.BatchV1().Jobs(c.JobDestinationNamespace)
	log.Info("Creating job " + jobObject.Name)
	_, err = jobsClient.Create(context.TODO(), jobObject, metav1.CreateOptions{})
	if err != nil {
		log.Error("error creating job: ", zap.String("error", err.Error()))
		return err
	}
	log.Info("Job " + jobObject.Name + " created successfully")
	return nil
}

// AddLabelsAsEnvVars adds alert labels as environment variables to the job
func AddLabelsAsEnvVars(jobObject *batchv1.Job, alert models.Alert) {
	log.Debug("Adding labels as environment variables")
	for labelkey, labelvalue := range alert.Labels {
		jobObject.Spec.Template.Spec.Containers[0].Env = append(
			jobObject.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "OPENFERO_" + strings.ToUpper(labelkey), Value: labelvalue},
		)
	}
}

// CheckJobTTL checks if TTL is set for the job
func CheckJobTTL(jobObject *batchv1.Job) bool {
	return jobObject.Spec.TTLSecondsAfterFinished != nil
}

// AddJobTTL adds TTL to the job
func AddJobTTL(jobObject *batchv1.Job) {
	ttl := int32(300)
	jobObject.Spec.TTLSecondsAfterFinished = &ttl
}

// CheckJobLabels checks if job labels match the label selector
func CheckJobLabels(jobObject *batchv1.Job, labelSelector *metav1.LabelSelector) bool {
	for key, value := range labelSelector.MatchLabels {
		if jobObject.Labels[key] != value {
			return false
		}
	}
	return true
}

// AddJobLabels adds labels to the job
func AddJobLabels(jobObject *batchv1.Job, labelSelector *metav1.LabelSelector) {
	jobObject.Labels = make(map[string]string)
	for key, value := range labelSelector.MatchLabels {
		jobObject.Labels[key] = value
	}
}

// GetJobFromConfigMap extracts a job definition from a ConfigMap
func GetJobFromConfigMap(configMap *corev1.ConfigMap, alertname string) (*batchv1.Job, error) {
	jobDefinition := configMap.Data[alertname]

	if jobDefinition == "" {
		return nil, fmt.Errorf("could not find a data block with the key %s in the configmap", alertname)
	}

	// Convert YAML to JSON
	jsonBytes, err := yaml.YAMLToJSON([]byte(jobDefinition))
	if err != nil {
		return nil, fmt.Errorf("error while converting YAML job definition to JSON: %v", err)
	}

	// Unmarshal JSON to Job object
	jobObject := &batchv1.Job{}
	if err := json.Unmarshal(jsonBytes, jobObject); err != nil {
		return nil, fmt.Errorf("error while using unmarshal on received job: %v", err)
	}

	// Adding randomString to avoid name conflict
	randomstring := utils.StringWithCharset(5, utils.Charset)
	jobObject.SetName(jobObject.Name + "-" + randomstring)

	return jobObject, nil
}
