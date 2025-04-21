package services

import (
	"strings"

	"github.com/OpenFero/openfero/pkg/alertstore"
	"github.com/OpenFero/openfero/pkg/kubernetes"
	log "github.com/OpenFero/openfero/pkg/logging"
	"github.com/OpenFero/openfero/pkg/metadata"
	"github.com/OpenFero/openfero/pkg/models"
	"github.com/OpenFero/openfero/pkg/utils"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

// CheckAlertStatus checks if alert status is valid
func CheckAlertStatus(status string) bool {
	return status == "resolved" || status == "firing"
}

// SaveAlert saves an alert to the alertstore
func SaveAlert(alertStore alertstore.Store, alert models.Alert, status string) {
	log.Debug("Saving alert in alert store",
		zap.String("alertname", alert.Labels["alertname"]),
		zap.String("status", status))

	// Convert to alertstore.Alert type
	storeAlert := alert.ToAlertStoreAlert()
	err := alertStore.SaveAlert(storeAlert, status)
	if err != nil {
		log.Error("Failed to save alert",
			zap.String("alertname", alert.Labels["alertname"]),
			zap.String("status", status),
			zap.Error(err))
	}
}

// SaveAlertWithJobInfo saves an alert to the alertstore with job information
func SaveAlertWithJobInfo(alertStore alertstore.Store, alert models.Alert, status string, jobInfo *alertstore.JobInfo) {
	log.Debug("Saving alert in alert store with job info",
		zap.String("alertname", alert.Labels["alertname"]),
		zap.String("status", status),
		zap.String("jobName", jobInfo.JobName))

	// Convert to alertstore.Alert type
	storeAlert := alert.ToAlertStoreAlert()
	err := alertStore.SaveAlertWithJobInfo(storeAlert, status, jobInfo)
	if err != nil {
		log.Error("Failed to save alert with job info",
			zap.String("alertname", alert.Labels["alertname"]),
			zap.String("status", status),
			zap.Error(err))
	}
}

// CreateResponseJob creates a response job for an alert
func CreateResponseJob(client *kubernetes.Client, alertStore alertstore.Store, alert models.Alert, status string) {
	alertname := utils.SanitizeInput(alert.Labels["alertname"])
	responsesConfigmap := strings.ToLower("openfero-" + alertname + "-" + status)
	log.Debug("Loading alert response configmap",
		zap.String("configmap", responsesConfigmap),
		zap.String("alertname", alertname),
		zap.String("status", status))

	// Get the configmap from the store
	obj, exists, err := client.ConfigMapStore.GetByKey(client.ConfigmapNamespace + "/" + responsesConfigmap)
	if err != nil {
		log.Error("Error getting configmap from store",
			zap.String("configmap", responsesConfigmap),
			zap.String("namespace", client.ConfigmapNamespace),
			zap.Error(err))
		// Save alert without job info since we couldn't get the configmap
		SaveAlert(alertStore, alert, status)
		return
	}
	if !exists {
		log.Error("Configmap not found in store",
			zap.String("configmap", responsesConfigmap),
			zap.String("namespace", client.ConfigmapNamespace),
			zap.String("alertname", alertname))
		// Save alert without job info since the configmap doesn't exist
		SaveAlert(alertStore, alert, status)
		return
	}

	configMap := obj.(*corev1.ConfigMap)

	// Get job from configmap
	jobObject, err := kubernetes.GetJobFromConfigMap(configMap, alertname)
	if err != nil {
		log.Error("Failed to get job from configmap",
			zap.String("configmap", responsesConfigmap),
			zap.String("alertname", alertname),
			zap.Error(err))
		// Save alert without job info since we couldn't get the job
		SaveAlert(alertStore, alert, status)
		return
	}

	// Adding alert labels to job
	kubernetes.AddLabelsAsEnvVars(jobObject, alert)
	log.Debug("Added alert labels as environment variables to job",
		zap.String("job", jobObject.Name),
		zap.String("alertname", alertname))

	// Adding TTL to job if it is not already set
	if !kubernetes.CheckJobTTL(jobObject) {
		kubernetes.AddJobTTL(jobObject)
		log.Debug("Added TTL to job", zap.String("job", jobObject.Name))
	}

	// Adding labels to job if they are not already set
	if !kubernetes.CheckJobLabels(jobObject, client.LabelSelector) {
		kubernetes.AddJobLabels(jobObject, client.LabelSelector)
		log.Debug("Added labels to job",
			zap.String("job", jobObject.Name),
			zap.Any("labelSelector", client.LabelSelector))
	}

	// Create the job
	err = client.CreateRemediationJob(jobObject)
	if err != nil {
		log.Error("Failed to create remediation job",
			zap.String("job", jobObject.Name),
			zap.String("alertname", alertname),
			zap.Error(err))
		metadata.JobsFailedTotal.Inc()
		// Save alert without job info since job creation failed
		SaveAlert(alertStore, alert, status)
		return
	}

	log.Info("Successfully created remediation job",
		zap.String("job", jobObject.Name),
		zap.String("alertname", alertname),
		zap.String("status", status))
	metadata.JobsCreatedTotal.Inc()

	// Create job info for the alert
	jobInfo := &alertstore.JobInfo{
		ConfigMapName: responsesConfigmap,
		JobName:       jobObject.Name,
		Image:         jobObject.Spec.Template.Spec.Containers[0].Image,
	}

	// Save the alert with job info
	SaveAlertWithJobInfo(alertStore, alert, status, jobInfo)
}
