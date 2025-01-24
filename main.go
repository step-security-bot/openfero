package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/OpenFero/openfero/pkg/logging"
	"github.com/OpenFero/openfero/pkg/metadata"
	"github.com/ghodss/yaml"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const contentType = "Content-Type"
const applicationJSON = "application/json"

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type (
	jobInfo struct {
		ConfigMapName string
		JobName       string
		Image         string
	}

	hookMessage struct {
		Version           string            `json:"version"`
		GroupKey          string            `json:"groupKey"`
		Status            string            `json:"status"`
		Receiver          string            `json:"receiver"`
		GroupLabels       map[string]string `json:"groupLabels"`
		CommonLabels      map[string]string `json:"commonLabels"`
		CommonAnnotations map[string]string `json:"commonAnnotations"`
		ExternalURL       string            `json:"externalURL"`
		Alerts            []alert           `json:"alerts"`
	}

	alert struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt,omitempty"`
		EndsAt      string            `json:"EndsAt,omitempty"`
	}

	clientsetStruct struct {
		clientset               kubernetes.Clientset
		jobDestinationNamespace string
		configmapNamespace      string
		configMapStore          cache.Store
		jobStore                cache.Store
	}
)

var alertStore []alert

const charset = "abcdefghijklmnopqrstuvwxyz0123456789"

func initKubeClient(kubeconfig *string) *kubernetes.Clientset {

	//use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatal("Could not read k8s configuration: %s", zap.String("error", err.Error()))
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("Could not create k8s client: %s", zap.String("error", err.Error()))
	}

	return clientset
}

func initConfigMapInformer(clientset *kubernetes.Clientset, configmapNamespace string) cache.Store {
	// Create informer factory
	configMapfactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		time.Hour*1,
		informers.WithNamespace(configmapNamespace),
	)

	// Get ConfigMap informer
	configMapInformer := configMapfactory.Core().V1().ConfigMaps().Informer()

	// Add event handlers to configMap informer
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

	// Start configMap informer
	go configMapfactory.Start(context.Background().Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(context.Background().Done(), configMapInformer.HasSynced) {
		log.Fatal("Failed to sync ConfigMap cache")
	}

	return configMapInformer.GetStore()

}

func initJobInformer(clientset *kubernetes.Clientset, jobDestinationNamespace string, labelSelector metav1.LabelSelector) cache.Store {
	// Create informer factory
	jobFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		time.Hour*1,
		informers.WithNamespace(jobDestinationNamespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = metav1.FormatLabelSelector(&labelSelector)
		}),
	)

	// Get Job informer
	jobInformer := jobFactory.Batch().V1().Jobs().Informer()

	// Add event handlers to job informer
	// Add job event handlers
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

	// Start job informer
	go jobFactory.Start(context.Background().Done())

	// Wait for job cache sync
	if !cache.WaitForCacheSync(context.Background().Done(), jobInformer.HasSynced) {
		log.Fatal("Failed to sync Job cache")
	}

	return jobInformer.GetStore()

}

func main() {

	// Parse command line arguments
	addr := flag.String("addr", ":8080", "address to listen for webhook")
	logLevel := flag.String("logLevel", "info", "log level")
	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	configmapNamespace := flag.String("configmapNamespace", "", "Kubernetes namespace where jobs are defined")
	jobDestinationNamespace := flag.String("jobDestinationNamespace", "", "Kubernetes namespace where jobs will be created")
	readTimeout := flag.Int("readTimeout", 5, "read timeout in seconds")
	writeTimeout := flag.Int("writeTimeout", 10, "write timeout in seconds")
	alertStoreSize := flag.Int("alertStoreSize", 10, "size of the alert store")

	flag.Parse()

	// Set the alert store size
	alertStore = make([]alert, 0, *alertStoreSize)

	// configure log
	var cfg zap.Config
	switch strings.ToLower(*logLevel) {
	case "debug":
		cfg = zap.NewDevelopmentConfig()
	case "info":
		cfg = zap.NewProductionConfig()
	default:
		log.Fatal("Invalid log level specified")
	}

	// activate json logging
	if log.SetConfig(cfg) != nil {
		log.Fatal("Could not set log configuration")
	}

	log.Info("Starting OpenFero", zap.String("version", version), zap.String("commit", commit), zap.String("date", date))

	// Use the in-cluster config to create a kubernetes client
	clientset := initKubeClient(kubeconfig)
	defaultNamespaceLocation := "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	currentNamespace := ""

	//Check if running in-cluster or out-of-cluster
	_, err := rest.InClusterConfig()
	if err != nil {
		log.Debug("Using out of cluster configuration")
		// Extract the current namespace from the client config
		currentNamespace, _, err = clientcmd.DefaultClientConfig.Namespace()
		if err != nil {
			log.Fatal("Current kubernetes namespace could not be found", zap.String("error", err.Error()))
		}
	} else {
		log.Debug("Using in cluster configuration")
		// Extract the current namespace from the mounted secrets
		if _, err := os.Stat(defaultNamespaceLocation); os.IsNotExist(err) {
			log.Fatal("Current kubernetes namespace could not be found", zap.String("error", err.Error()))
		}
		namespaceDat, err := os.ReadFile(defaultNamespaceLocation)
		if err != nil {
			log.Fatal("Couldn't read from "+defaultNamespaceLocation, zap.String("error", err.Error()))
		}
		currentNamespace = string(namespaceDat)
	}

	if *configmapNamespace == "" {
		configmapNamespace = &currentNamespace
	}

	if *jobDestinationNamespace == "" {
		jobDestinationNamespace = &currentNamespace
	}

	// Create label selector for openfero ConfigMaps
	labelSelector := metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": "openfero",
		},
	}

	// Create informer factory for configmaps
	configMapInformer := initConfigMapInformer(clientset, *configmapNamespace)
	// Create informer factory for jobs
	jobInformer := initJobInformer(clientset, *jobDestinationNamespace, labelSelector)

	server := &clientsetStruct{
		clientset:               *clientset,
		jobDestinationNamespace: *jobDestinationNamespace,
		configmapNamespace:      *configmapNamespace,
		configMapStore:          configMapInformer,
		jobStore:                jobInformer,
	}

	//register metrics and set prometheus handler
	metadata.AddMetricsToPrometheusRegistry()
	http.Handle(metadata.MetricsPath, promhttp.Handler())

	log.Info("Starting webhook receiver")
	http.HandleFunc("GET /healthz", server.healthzGetHandler)
	http.HandleFunc("GET /readiness", server.readinessGetHandler)
	http.HandleFunc("GET /alertStore", server.alertStoreGetHandler)
	http.HandleFunc("GET /alerts", server.alertsGetHandler)
	http.HandleFunc("POST /alerts", server.alertsPostHandler)
	http.HandleFunc("GET /ui", uiHandler)
	http.HandleFunc("GET /ui/jobs", server.jobsUIHandler)
	http.HandleFunc("GET /assets/", assetsHandler)

	srv := &http.Server{
		Addr:         *addr,
		ReadTimeout:  time.Duration(*readTimeout) * time.Second,
		WriteTimeout: time.Duration(*writeTimeout) * time.Second,
	}

	log.Info("Starting server on " + *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal("error starting server: ", zap.String("error", err.Error()))
	}
}

// Use math/rand to generate a random string of a given length and charset
func stringWithCharset(length int, charset string) string {
	randombytes := make([]byte, length)
	for i := range randombytes {
		num := rand.Intn(len(charset))
		randombytes[i] = charset[num]
	}

	return string(randombytes)
}

// handling healthness probe
func (server *clientsetStruct) healthzGetHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(contentType, applicationJSON)
	w.WriteHeader(http.StatusOK)
}

// handling readiness probe
func (server *clientsetStruct) readinessGetHandler(w http.ResponseWriter, r *http.Request) {
	_, err := server.clientset.CoreV1().ConfigMaps(server.configmapNamespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Error("error listing configmaps: ", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set(contentType, applicationJSON)
	w.WriteHeader(http.StatusOK)
}

// Handling get requests to listen received alerts
func (server *clientsetStruct) alertsGetHandler(httpwriter http.ResponseWriter, httprequest *http.Request) {
	// Alertmanager expects an 200 OK response, otherwise send_resolved will never work
	enc := json.NewEncoder(httpwriter)
	httpwriter.Header().Set(contentType, applicationJSON)
	httpwriter.WriteHeader(http.StatusOK)

	if err := enc.Encode("OK"); err != nil {
		log.Error("error encoding messages: ", zap.String("error", err.Error()))
		http.Error(httpwriter, "", http.StatusInternalServerError)
	}
}

// Handling the Alertmanager Post-Requests
func (server *clientsetStruct) alertsPostHandler(httpwriter http.ResponseWriter, httprequest *http.Request) {

	dec := json.NewDecoder(httprequest.Body)
	defer httprequest.Body.Close()

	message := hookMessage{}
	if err := dec.Decode(&message); err != nil {
		log.Error("error decoding message: ", zap.String("error", err.Error()))
		http.Error(httpwriter, "invalid request body", http.StatusBadRequest)
		return
	}

	status := sanitizeInput(message.Status)
	alertcount := len(message.Alerts)

	log.Debug(status + " webhook received with " + fmt.Sprint(alertcount) + " alerts")

	if !checkAlertStatus(status) {
		log.Warn("Status of alert was neither firing nor resolved, stop creating a response job.")
		return
	}

	log.Debug("Creating response job for " + fmt.Sprint(alertcount) + " alerts")

	for _, alert := range message.Alerts {
		go server.createResponseJob(alert, status, httpwriter)
	}

}

func checkAlertStatus(status string) bool {
	return status == "resolved" || status == "firing"
}

func sanitizeInput(input string) string {
	input = strings.ReplaceAll(input, "\n", "")
	input = strings.ReplaceAll(input, "\r", "")
	return input
}

func (server *clientsetStruct) createResponseJob(alert alert, status string, _ http.ResponseWriter) {
	server.saveAlert(alert)
	alertname := sanitizeInput(alert.Labels["alertname"])
	responsesConfigmap := strings.ToLower("openfero-" + alertname + "-" + status)
	log.Debug("Try to load configmap " + responsesConfigmap)

	// Get ConfigMap from store instead of API
	obj, exists, err := server.configMapStore.GetByKey(server.configmapNamespace + "/" + responsesConfigmap)
	if err != nil {
		log.Error("error getting configmap from store: ", zap.String("error", err.Error()))
		return
	}
	if !exists {
		log.Error("configmap not found in store")
		return
	}

	configMap := obj.(*v1.ConfigMap)

	jobDefinition := configMap.Data[alertname]

	if jobDefinition == "" {
		log.Error("Could not find a data block with the key " + alertname + " in the configmap.")
		return
	}
	yamlJobDefinition := []byte(jobDefinition)

	// yamlJobDefinition contains a []byte of the yaml job spec
	// convert the yaml to json so it works with Unmarshal
	jsonBytes, err := yaml.YAMLToJSON(yamlJobDefinition)
	if err != nil {
		log.Error("error while converting YAML job definition to JSON: ", zap.String("error", err.Error()))
		return
	}
	randomstring := stringWithCharset(5, charset)

	jobObject := &batchv1.Job{}
	err = json.Unmarshal(jsonBytes, jobObject)
	if err != nil {
		log.Error("Error while using unmarshal on received job: ", zap.String("error", err.Error()))
		return
	}

	// Adding randomString to avoid name conflict
	jobObject.SetName(jobObject.Name + "-" + randomstring)

	// Adding alert labels to job
	addLabelsAsEnvVars(jobObject, alert)

	// Adding TTL to job if it is not already set
	if !checkJobTTL(jobObject) {
		addJobTTL(jobObject)
	}

	// Adding labels to job if they are not already set
	if !checkJobLabels(jobObject) {
		addJobLabels(jobObject)
	}

	// Create the job
	err = server.createRemediationJob(jobObject)
	if err != nil {
		log.Error("error creating job: ", zap.String("error", err.Error()))
		return
	}

}

func (server *clientsetStruct) createRemediationJob(jobObject *batchv1.Job) error {
	// Check if job already exists
	_, exists, err := server.jobStore.GetByKey(server.jobDestinationNamespace + "/" + jobObject.Name)
	if err != nil {
		log.Error("error checking job existence: ", zap.String("error", err.Error()))
		return err
	}
	if exists {
		return fmt.Errorf("job %s already exists", jobObject.Name)
	}

	// Create job
	jobsClient := server.clientset.BatchV1().Jobs(server.jobDestinationNamespace)
	log.Info("Creating job " + jobObject.Name)
	_, err = jobsClient.Create(context.TODO(), jobObject, metav1.CreateOptions{})
	if err != nil {
		log.Error("error creating job: ", zap.String("error", err.Error()))
		return err
	}
	log.Info("Job " + jobObject.Name + " created successfully")
	return nil
}

func addLabelsAsEnvVars(jobObject *batchv1.Job, alert alert) {
	// Adding Labels as Environment variables
	log.Debug("Adding labels as environment variables")
	for labelkey, labelvalue := range alert.Labels {
		jobObject.Spec.Template.Spec.Containers[0].Env = append(jobObject.Spec.Template.Spec.Containers[0].Env, v1.EnvVar{Name: "OPENFERO_" + strings.ToUpper(labelkey), Value: labelvalue})
	}
}

func checkJobTTL(jobObject *batchv1.Job) bool {
	return jobObject.Spec.TTLSecondsAfterFinished != nil
}

func addJobTTL(jobObject *batchv1.Job) {
	ttl := int32(300)
	jobObject.Spec.TTLSecondsAfterFinished = &ttl
}

func checkJobLabels(jobObject *batchv1.Job) bool {
	return jobObject.Labels != nil
}

func addJobLabels(jobObject *batchv1.Job) {
	jobObject.Labels = make(map[string]string)
	jobObject.Labels["app"] = "openfero"
}

// function which saves the alert in the alertStore
func (server *clientsetStruct) saveAlert(alert alert) {
	if len(alertStore) < cap(alertStore) {
		alertStore = append(alertStore, alert)
	} else {
		log.Debug("Alert store is full, dropping oldest alert")
		copy(alertStore, alertStore[1:])
		alertStore[len(alertStore)-1] = alert
	}
}

// function which filters alerts based on the query
func filterAlerts(alerts []alert, query string) []alert {
	var filteredAlerts []alert
	// Return all alerts if query is empty
	if query == "" {
		return alerts
	}

	for _, alert := range alerts {
		if alertMatchesQuery(alert, query) {
			filteredAlerts = append(filteredAlerts, alert)
		}
	}
	return filteredAlerts
}

func alertMatchesQuery(alert alert, query string) bool {
	query = strings.ToLower(query)
	alertname := strings.ToLower(alert.Labels["alertname"])

	// Create a channel to receive match results
	matchChan := make(chan bool, 3)

	// Check alertname concurrently
	go func() {
		matchChan <- strings.Contains(alertname, query)
	}()

	// Check labels concurrently
	go func() {
		for _, value := range alert.Labels {
			if strings.Contains(strings.ToLower(value), query) {
				matchChan <- true
				return
			}
		}
		matchChan <- false
	}()

	// Check annotations concurrently
	go func() {
		for _, value := range alert.Annotations {
			if strings.Contains(strings.ToLower(value), query) {
				matchChan <- true
				return
			}
		}
		matchChan <- false
	}()

	// Collect results from all goroutines
	for i := 0; i < 3; i++ {
		if <-matchChan {
			return true
		}
	}

	return false
}

func assetsHandler(w http.ResponseWriter, r *http.Request) {
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
	path, err := verifyPath(r.URL.Path)
	if err != nil {
		http.Error(w, "Invalid path specified", http.StatusBadRequest)
		return
	}

	log.Debug("Called asset " + r.URL.Path + " serves Filesystem asset: " + path)
	// serve assets from the web/assets directory
	http.ServeFile(w, r, path)
}

// verifyPath verifies and evaluates the given path to ensure it is safe and valid.
// It checks if the path is within the trusted root directory and evaluates any symbolic links.
// If the path is unsafe or invalid, it returns an error.
// Otherwise, it returns the evaluated path.
func verifyPath(path string) (string, error) {
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

// function which provides alerts array to the getHandler
func (server *clientsetStruct) alertStoreGetHandler(w http.ResponseWriter, r *http.Request) {
	// Get search query parameter
	query := r.URL.Query().Get("q")

	alerts := filterAlerts(alertStore, query)

	w.Header().Set(contentType, applicationJSON)
	err := json.NewEncoder(w).Encode(alerts)
	if err != nil {
		log.Error("error encoding alerts: ", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
	}
}

// function which provides the UI to the user
func uiHandler(w http.ResponseWriter, r *http.Request) {
	var alerts []alert
	w.Header().Set(contentType, "text/html")
	//Parse the templates in web/templates/
	tmpl, err := template.ParseFiles(
		"web/templates/alertStore.html.templ",
		"web/templates/navbar.html.templ",
	)
	if err != nil {
		log.Error("error parsing templates: ", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
	}

	query := r.URL.Query().Get("q")

	alerts = getAlerts(query)

	data := struct {
		Title      string
		ShowSearch bool
		Alerts     []alert
	}{
		Title:      "Alerts",
		ShowSearch: true,
		Alerts:     alerts,
	}

	//Execute the templates
	err = tmpl.Execute(w, data)
	if err != nil {
		log.Error("error executing templates: ", zap.String("error", err.Error()))
		http.Error(w, "", http.StatusInternalServerError)
	}
}

// function which gets alerts from the alertStore
func getAlerts(query string) []alert {
	resp, err := http.Get("http://localhost:8080/alertStore?q=" + query)
	if err != nil {
		log.Error("error getting alerts: ", zap.String("error", err.Error()))
	}
	defer resp.Body.Close()
	var alerts []alert
	err = json.NewDecoder(resp.Body).Decode(&alerts)
	if err != nil {
		log.Error("error decoding alerts: ", zap.String("error", err.Error()))
	}
	return alerts
}

func (server *clientsetStruct) jobsUIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(contentType, "text/html")

	// Get all ConfigMaps from store
	var jobInfos []jobInfo
	for _, obj := range server.configMapStore.List() {
		configMap := obj.(*v1.ConfigMap)

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
				jobInfos = append(jobInfos, jobInfo{
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
		Jobs       []jobInfo
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
