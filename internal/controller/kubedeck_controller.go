package controller

import (
	"context"
	"encoding/json"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrlv1 "nikcorp.ru/kubedeck/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// KubedeckReconciler reconciles a Kubedeck object
type KubedeckReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *rest.Config
}

// Separate logger for the web server
var webServerLog = logf.Log.WithName("kubedeck-webserver")

// +kubebuilder:rbac:groups=*,resources=*,verbs=*
// +kubebuilder:rbac:groups=ctrl.nikcorp.ru,resources=kubedecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ctrl.nikcorp.ru,resources=kubedecks/finalizers,verbs=update
func (r *KubedeckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	// TODO(user): your logic here
	return ctrl.Result{}, nil
}

// --- Handler Helper ---
func writeJsonResponse(w http.ResponseWriter, data interface{}, resourceName string, errVal error) {
	if errVal != nil {
		webServerLog.Error(errVal, "Failed to list "+resourceName)
		http.Error(w, "Failed to list "+resourceName+": "+errVal.Error(), http.StatusInternalServerError)
		return
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		webServerLog.Error(err, "Failed to marshal "+resourceName+" to JSON")
		http.Error(w, "Failed to marshal "+resourceName+" to JSON: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, writeErr := w.Write(jsonData)
	if writeErr != nil {
		webServerLog.Error(writeErr, "Failed to write response for "+resourceName)
	}
}

// --- Handlers for Kubernetes Resources ---
// startWebServer starts the HTTP server in a new goroutine.
func (r *KubedeckReconciler) startWebServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/pods", r.handlePodsRequest)                     //ok
	mux.HandleFunc("/pv", r.handlePVRequest)                         //ok
	mux.HandleFunc("/pvc", r.handlePVCRequest)                       //ok
	mux.HandleFunc("/configmaps", r.handleConfigMapsRequest)         //ok
	mux.HandleFunc("/services", r.handleServicesRequest)             //ok
	mux.HandleFunc("/ingresses", r.handleIngressesRequest)           //ok
	mux.HandleFunc("/deployments", r.handleDeploymentsRequest)       //ok
	mux.HandleFunc("/statefulsets", r.handleStatefulSetsRequest)     //ok
	mux.HandleFunc("/daemonsets", r.handleDaemonSetsRequest)         //ok
	mux.HandleFunc("/replicasets", r.handleReplicaSetsRequest)       //ok
	mux.HandleFunc("/namespaces", r.handleNamespacesRequest)         //ok
	mux.HandleFunc("/nodes", r.handleNodesRequest)                   //ok
	mux.HandleFunc("/storageclasses", r.handleStorageClassesRequest) //ok
	mux.HandleFunc("/logs", r.handlePodLogsRequest)                  //ok
	// Pods CRUD
	mux.HandleFunc("/pods/create", r.HandleCreatePod)
	mux.HandleFunc("/pods/update", r.HandleUpdatePod)
	mux.HandleFunc("/pods/delete", r.HandleDeletePod)
	// ConfigMaps CRUD
	mux.HandleFunc("/configmaps/create", r.HandleCreateConfigMap)
	mux.HandleFunc("/configmaps/update", r.HandleUpdateConfigMap)
	mux.HandleFunc("/configmaps/delete", r.HandleDeleteConfigMap)
	// Deployments CRUD
	mux.HandleFunc("/deployments/create", r.HandleCreateDeployment)
	mux.HandleFunc("/deployments/update", r.HandleUpdateDeployment)
	mux.HandleFunc("/deployments/delete", r.HandleDeleteDeployment)
	// Новые обработчики для метрик
	mux.HandleFunc("/metrics/cluster", r.handleClusterMetricsRequest)
	mux.HandleFunc("/metrics/pods", r.handlePodMetricsRequest)
	// PV CRUD
	mux.HandleFunc("/pv/create", r.HandleCreatePersistentVolume)
	mux.HandleFunc("/pv/update", r.HandleUpdatePersistentVolume)
	mux.HandleFunc("/pv/delete", r.HandleDeletePersistentVolume)
	// DaemonSets CRUD
	mux.HandleFunc("/daemonsets/create", r.HandleCreateDaemonSet)
	mux.HandleFunc("/daemonsets/update", r.HandleUpdateDaemonSet)
	mux.HandleFunc("/daemonsets/delete", r.HandleDeleteDaemonSet)
	// StatefulSet CRUD
	mux.HandleFunc("/statefulsets/create", r.HandleCreateStatefulSet)
	mux.HandleFunc("/statefulsets/update", r.HandleUpdateStatefulSet)
	mux.HandleFunc("/statefulsets/delete", r.HandleDeleteStatefulSet)
	// PVC CRUD
	mux.HandleFunc("/pvc/create", r.HandleCreatePersistentVolumeClaim)
	mux.HandleFunc("/pvc/update", r.HandleUpdatePersistentVolumeClaim)
	mux.HandleFunc("/pvc/delete", r.HandleDeletePersistentVolumeClaim)
	// Service CRUD
	mux.HandleFunc("/services/create", r.HandleCreateService)
	mux.HandleFunc("/services/update", r.HandleUpdateService)
	mux.HandleFunc("/services/delete", r.HandleDeleteService)
	// Ingress CRUD
	mux.HandleFunc("/ingresses/create", r.HandleCreateIngress)
	mux.HandleFunc("/ingresses/update", r.HandleUpdateIngress)
	mux.HandleFunc("/ingresses/delete", r.HandleDeleteIngress)
	// StorageClass CRUD
	mux.HandleFunc("/storageclasses/create", r.HandleCreateStorageClass)
	mux.HandleFunc("/storageclasses/update", r.HandleUpdateStorageClass)
	mux.HandleFunc("/storageclasses/delete", r.HandleDeleteStorageClass)
	// Node CRUD
	mux.HandleFunc("/nodes/update", r.HandleUpdateNode)
	mux.HandleFunc("/nodes/delete", r.HandleDeleteNode)
	// ReplicaSet CRUD
	mux.HandleFunc("/replicasets/create", r.HandleCreateReplicaSet)
	mux.HandleFunc("/replicasets/update", r.HandleUpdateReplicaSet)
	mux.HandleFunc("/replicasets/delete", r.HandleDeleteReplicaSet)
	// Namespaces CRUD (пример для кластерного)
	mux.HandleFunc("/namespaces/create", r.HandleCreateNamespace)
	webServerLog.Info("Starting web server", "port", 8999)
	if err := http.ListenAndServe(":8999", mux); err != nil {
		webServerLog.Error(err, "Failed to start web server")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubedeckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Start the web server in a goroutine.
	go r.startWebServer()
	return ctrl.NewControllerManagedBy(mgr).
		For(&ctrlv1.Kubedeck{}). // Assuming Kubedeck is your primary CRD
		Named("kubedeck").
		Complete(r)
}
