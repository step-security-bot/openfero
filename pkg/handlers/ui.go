package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	log "github.com/OpenFero/openfero/pkg/logging"
	"github.com/OpenFero/openfero/pkg/models"
	"github.com/ghodss/yaml"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UIHandler handles GET requests to /
func UIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(ContentTypeHeader, "text/html")

	// Parse templates
	tmpl, err := template.ParseFiles(
		"web/templates/alertStore.html.templ",
		"web/templates/navbar.html.templ",
	)
	if err != nil {
		log.Error("error parsing templates: ", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	query := r.URL.Query().Get("q")
	alerts := GetAlerts(query)

	data := struct {
		Title      string
		ShowSearch bool
		Alerts     []models.AlertStoreEntry
	}{
		Title:      "Alerts",
		ShowSearch: true,
		Alerts:     alerts,
	}

	// Execute templates
	if err = tmpl.Execute(w, data); err != nil {
		log.Error("error executing templates: ", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
	}
}

// GetAlerts fetches alerts from the alert store
func GetAlerts(query string) []models.AlertStoreEntry {
	resp, err := http.Get("http://localhost:8080/alertStore?q=" + query)
	if err != nil {
		log.Error("error getting alerts: ", zap.String("error", err.Error()))
		return nil
	}
	defer resp.Body.Close()

	var alerts []models.AlertStoreEntry
	err = json.NewDecoder(resp.Body).Decode(&alerts)
	if err != nil {
		log.Error("error decoding alerts: ", zap.String("error", err.Error()))
	}
	return alerts
}

// JobsUIHandler handles GET requests to /jobs
func (s *Server) JobsUIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(ContentTypeHeader, "text/html")

	// Get all ConfigMaps from store
	var jobInfos []models.JobInfo
	for _, obj := range s.KubeClient.ConfigMapStore.List() {
		configMap := obj.(*corev1.ConfigMap)

		// Process each job definition in ConfigMap
		for name, jobDef := range configMap.Data {
			// Parse YAML job definition
			yamlJobDefinition := []byte(jobDef)
			jsonBytes, err := yaml.YAMLToJSON(yamlJobDefinition)
			if err != nil {
				log.Error("error converting YAML to JSON", zap.String("error", err.Error()))
				continue
			}

			jobObject := &batchv1.Job{}
			if err := json.Unmarshal(jsonBytes, jobObject); err != nil {
				log.Error("error unmarshaling job definition", zap.String("error", err.Error()))
				continue
			}

			// Extract container image
			if len(jobObject.Spec.Template.Spec.Containers) > 0 {
				jobInfos = append(jobInfos, models.JobInfo{
					ConfigMapName: configMap.Name,
					JobName:       name,
					Image:         jobObject.Spec.Template.Spec.Containers[0].Image,
				})
			}
		}
	}

	// Parse and execute template
	tmpl, err := template.ParseFiles(
		"web/templates/jobs.html.templ",
		"web/templates/navbar.html.templ",
	)
	if err != nil {
		log.Error("error parsing template", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	data := struct {
		Title      string
		ShowSearch bool
		Jobs       []models.JobInfo
	}{
		Title:      "Jobs",
		ShowSearch: false,
		Jobs:       jobInfos,
	}

	if err := tmpl.Execute(w, data); err != nil {
		log.Error("error executing template", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
	}
}

// AssetsHandler serves static assets
func AssetsHandler(w http.ResponseWriter, r *http.Request) {
	log.Debug("Called asset " + r.URL.Path)
	// set content type based on file extension
	contentType := ""
	switch filepath.Ext(r.URL.Path) {
	case ".css":
		contentType = "text/css"
	case ".js":
		contentType = "application/javascript"
	}
	w.Header().Set("Content-Type", contentType)

	// sanitize the URL path to prevent path traversal
	path, err := VerifyPath(r.URL.Path)
	if err != nil {
		http.Error(w, "Invalid path specified", http.StatusBadRequest)
		return
	}

	log.Debug("Called asset " + r.URL.Path + " serves Filesystem asset: " + path)
	// serve assets from the web/assets directory
	http.ServeFile(w, r, path)
}

// VerifyPath verifies and evaluates the given path to ensure it is safe
func VerifyPath(path string) (string, error) {
	errmsg := "unsafe or invalid path specified"
	wd, err := os.Getwd()
	if err != nil {
		log.Error("Error getting working directory: ", zap.String("error", err.Error()))
		return "", errors.New(errmsg)
	}
	trustedRoot := filepath.Join(wd, "web")
	log.Debug("Trusted root directory: " + trustedRoot)

	// Clean the path to remove any .. or . elements
	cleanPath := filepath.Clean(path)
	// Join the trusted root and the cleaned path
	absPath, err := filepath.Abs(filepath.Join(trustedRoot, cleanPath))
	if err != nil || !strings.HasPrefix(absPath, trustedRoot) {
		log.Error("Error getting absolute path: ", zap.String("error", err.Error()))
		return "", errors.New(errmsg)
	}

	return absPath, nil
}

// HealthzGetHandler handles health status requests
func (s *Server) HealthzGetHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(ContentTypeHeader, ApplicationJSONVal)
	w.WriteHeader(http.StatusOK)
}

// ReadinessGetHandler handles readiness probe requests
func (s *Server) ReadinessGetHandler(w http.ResponseWriter, r *http.Request) {
	_, err := s.KubeClient.Clientset.CoreV1().ConfigMaps(s.KubeClient.ConfigmapNamespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Error("error listing configmaps: ", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set(ContentTypeHeader, ApplicationJSONVal)
	w.WriteHeader(http.StatusOK)
}
