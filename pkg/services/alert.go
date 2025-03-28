package services

import (
	"strings"

	"github.com/OpenFero/openfero/pkg/alertstore"
	"github.com/OpenFero/openfero/pkg/kubernetes"
	log "github.com/OpenFero/openfero/pkg/logging"
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
	log.Debug("Saving alert in alert store")
	// Convert to alertstore.Alert type
	storeAlert := alert.ToAlertStoreAlert()
	err := alertStore.SaveAlert(storeAlert, status)
	if err != nil {
		log.Error("Failed to save alert", zap.Error(err))
	}
}

// CreateResponseJob creates a response job for an alert
func CreateResponseJob(client *kubernetes.Client, alertStore alertstore.Store, alert models.Alert, status string) {
	// Save alert first
	SaveAlert(alertStore, alert, status)

	alertname := utils.SanitizeInput(alert.Labels["alertname"])
	responsesConfigmap := strings.ToLower("openfero-" + alertname + "-" + status)
	log.Debug("Try to load configmap " + responsesConfigmap)

	// Get the configmap from the store
	obj, exists, err := client.ConfigMapStore.GetByKey(client.ConfigmapNamespace + "/" + responsesConfigmap)
	if err != nil {
		log.Error("error getting configmap from store: ", zap.String("error", err.Error()))
		return
	}
	if !exists {
		log.Error("configmap not found in store")
		return
	}

	configMap := obj.(*corev1.ConfigMap)

	// Get job from configmap
	jobObject, err := kubernetes.GetJobFromConfigMap(configMap, alertname)
	if err != nil {
		log.Error(err.Error())
		return
	}

	// Adding alert labels to job
	kubernetes.AddLabelsAsEnvVars(jobObject, alert)

	// Adding TTL to job if it is not already set
	if !kubernetes.CheckJobTTL(jobObject) {
		kubernetes.AddJobTTL(jobObject)
	}

	// Adding labels to job if they are not already set
	if !kubernetes.CheckJobLabels(jobObject, client.LabelSelector) {
		kubernetes.AddJobLabels(jobObject, client.LabelSelector)
	}

	// Create the job
	err = client.CreateRemediationJob(jobObject)
	if err != nil {
		log.Error("error creating job: ", zap.String("error", err.Error()))
		return
	}
}
