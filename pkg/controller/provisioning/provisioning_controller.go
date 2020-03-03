package provisioning

import (
	"context"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	osconfigv1 "github.com/openshift/api/config/v1"
	metal3v1alpha1 "github.com/openshift/cluster-baremetal-operator/pkg/apis/metal3/v1alpha1"
)

var log = logf.Log.WithName("controller_provisioning")
var componentNamespace = "openshift-machine-api"

// OperatorConfig contains configuration for the metal3 Deployment
type OperatorConfig struct {
	TargetNamespace      string
	BaremetalControllers BaremetalControllers
}

type BaremetalControllers struct {
	BaremetalOperator         string
	Ironic                    string
	IronicInspector           string
	IronicIpaDownloader       string
	IronicMachineOsDownloader string
	IronicStaticIpManager     string
}

// Add creates a new Provisioning Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileProvisioning{
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
		config: &OperatorConfig{
			TargetNamespace: componentNamespace,
			BaremetalControllers: BaremetalControllers{
				BaremetalOperator:         os.Getenv("BAREMETAL_IMAGE"),
				Ironic:                    os.Getenv("IRONIC_IMAGE"),
				IronicInspector:           os.Getenv("IRONIC_INSPECTOR_IMAGE"),
				IronicIpaDownloader:       os.Getenv("IRONIC_IPA_DOWNLOADER_IMAGE"),
				IronicMachineOsDownloader: os.Getenv("IRONIC_MACHINE_OS_DOWNLOADER_IMAGE"),
				IronicStaticIpManager:     os.Getenv("IRONIC_STATIC_IP_MANAGER_IMAGE"),
			},
		},
	}

}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("provisioning-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Provisioning
	err = c.Watch(&source.Kind{Type: &metal3v1alpha1.Provisioning{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to our Deployment and Secret and requeue the owner Provisioning
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &metal3v1alpha1.Provisioning{},
	})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &metal3v1alpha1.Provisioning{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileProvisioning implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileProvisioning{}

// ReconcileProvisioning reconciles a Provisioning object
type ReconcileProvisioning struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	config *OperatorConfig
}

// Reconcile reads that state of the cluster for a Provisioning object and makes changes based on the state read
// and what is in the Provisioning.Spec
func (r *ReconcileProvisioning) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Provisioning")

	// provisioning.metal3.io is a singleton
	if request.Name != baremetalProvisioningCR {
		reqLogger.Info("Ignoring Provisioning.metal3.io without default name")
		return reconcile.Result{}, nil
	}

	infra := &osconfigv1.Infrastructure{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, infra)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Disable ourselves on platforms other than bare metal
	if infra.Status.Platform != osconfigv1.BareMetalPlatformType {
		err = syncClusterOperator(r.client, r.config.TargetNamespace, os.Getenv("OPERATOR_VERSION"), true, true)
		if err != nil {
			return reconcile.Result{}, err
		}
		// We're disabled; don't requeue
		return reconcile.Result{}, nil
	}

	// Fetch the Provisioning instance
	instance := &metal3v1alpha1.Provisioning{}
	err = r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Create a Secret needed for the Metal3 deployment
	secret := createMariadbPasswordSecret(r.config)
	if err := controllerutil.SetControllerReference(instance, secret, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	foundSecret := &corev1.Secret{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, foundSecret)
	if err != nil && errors.IsNotFound(err) {
		// Secret does not already exist. So, create one.
		reqLogger.Info("Creating a new Maridb password secret", "Secret.Namespace", secret.Namespace, "Deployment.Name", secret.Name)
		err := r.client.Create(context.TODO(), secret)
		if err != nil {
			return reconcile.Result{}, err
		}
	} else if err != nil {
		return reconcile.Result{}, err
	}

	// Define a new Deployment object
	deployment := newMetal3Deployment(r.config, getBaremetalProvisioningConfig(instance))
	if err := controllerutil.SetControllerReference(instance, deployment, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	// Check if this Deployment already exists
	found := &appsv1.Deployment{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new Deployment", "Deployment.Namespace", deployment.Namespace, "Deployment.Name", deployment.Name)
		err = r.client.Create(context.TODO(), deployment)
		if err != nil {
			return reconcile.Result{}, err
		}
	} else if err != nil {
		return reconcile.Result{}, err
	} else {
		reqLogger.Info("Skip reconcile: Deployment already exists", "Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)
	}

	err = syncClusterOperator(r.client, r.config.TargetNamespace, os.Getenv("OPERATOR_VERSION"), true, false)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Success; don't requeue
	return reconcile.Result{}, nil
}
