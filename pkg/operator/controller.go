package operator

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	metaapi "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	kubeset "k8s.io/client-go/kubernetes"
	appsset "k8s.io/client-go/kubernetes/typed/apps/v1"
	batchset "k8s.io/client-go/kubernetes/typed/batch/v1beta1"
	coreset "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacset "k8s.io/client-go/kubernetes/typed/rbac/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	configapiv1 "github.com/openshift/api/config/v1"
	imageregistryv1 "github.com/openshift/api/imageregistry/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	configset "github.com/openshift/client-go/config/clientset/versioned"
	configsetv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	regopset "github.com/openshift/client-go/imageregistry/clientset/versioned"
	regopinformers "github.com/openshift/client-go/imageregistry/informers/externalversions"
	routeset "github.com/openshift/client-go/route/clientset/versioned"
	routesetv1 "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	routeinformers "github.com/openshift/client-go/route/informers/externalversions"

	regopclient "github.com/openshift/cluster-image-registry-operator/pkg/client"
	"github.com/openshift/cluster-image-registry-operator/pkg/defaults"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource/object"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource/strategy"
	"github.com/openshift/cluster-image-registry-operator/pkg/storage"
)

const (
	kubeSystemNamespace   = "kube-system"
	workqueueKey          = "changes"
	defaultResyncDuration = 10 * time.Minute
)

type permanentError struct {
	Err    error
	Reason string
}

func newPermanentError(reason string, err error) error {
	return permanentError{
		Err:    err,
		Reason: reason,
	}
}

func (e permanentError) Error() string {
	return e.Err.Error()
}

// NewController returns a controller for openshift image registry objects.
//
// This controller keeps track of resources needed in order to have openshift
// internal registry working.
func NewController(kubeconfig *restclient.Config) (*Controller, error) {
	listers := &regopclient.Listers{}
	clients := &regopclient.Clients{}
	c := &Controller{
		kubeconfig: kubeconfig,
		generator:  resource.NewGenerator(kubeconfig, clients, listers),
		workqueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Changes"),
		listers:    listers,
		clients:    clients,
	}

	// Initial event to bootstrap CR if it doesn't exist.
	c.workqueue.AddRateLimited(workqueueKey)

	return c, nil
}

// Controller keeps track of openshift image registry components.
type Controller struct {
	kubeconfig *restclient.Config
	generator  *resource.Generator
	workqueue  workqueue.RateLimitingInterface
	listers    *regopclient.Listers
	clients    *regopclient.Clients
}

func (c *Controller) createOrUpdateResources(cr *imageregistryv1.Config) error {
	appendFinalizer(cr)

	err := verifyResource(cr)
	if err != nil {
		return newPermanentError("VerificationFailed", fmt.Errorf("unable to complete resource: %s", err))
	}

	err = c.generator.Apply(cr)
	if err == storage.ErrStorageNotConfigured {
		return newPermanentError("StorageNotConfigured", err)
	} else if err != nil {
		return err
	}

	return nil
}

func (c *Controller) sync() error {
	cr, err := c.listers.RegistryConfigs.Get(defaults.ImageRegistryResourceName)
	if err != nil {
		if errors.IsNotFound(err) {
			return c.Bootstrap()
		}
		return fmt.Errorf("failed to get %q registry operator resource: %s", defaults.ImageRegistryResourceName, err)
	}
	cr = cr.DeepCopy() // we don't want to change the cached version
	prevCR := cr.DeepCopy()

	if cr.ObjectMeta.DeletionTimestamp != nil {
		err = c.finalizeResources(cr)
		return err
	}

	var applyError error
	switch cr.Spec.ManagementState {
	case operatorapi.Removed:
		applyError = c.RemoveResources(cr)
	case operatorapi.Managed:
		applyError = c.createOrUpdateResources(cr)
	case operatorapi.Unmanaged:
		// ignore
	default:
		klog.Warningf("unknown custom resource state: %s", cr.Spec.ManagementState)
	}

	deploy, err := c.listers.Deployments.Get(defaults.ImageRegistryName)
	if errors.IsNotFound(err) {
		deploy = nil
	} else if err != nil {
		return fmt.Errorf("failed to get %q deployment: %s", defaults.ImageRegistryName, err)
	} else {
		deploy = deploy.DeepCopy() // make sure we won't corrupt the cached vesrion
	}

	c.syncStatus(cr, deploy, applyError)

	metadataChanged := strategy.Metadata(&prevCR.ObjectMeta, &cr.ObjectMeta)
	specChanged := !reflect.DeepEqual(prevCR.Spec, cr.Spec)
	if metadataChanged || specChanged {
		difference, err := object.DiffString(prevCR, cr)
		if err != nil {
			klog.Errorf("unable to calculate difference in %s: %s", utilObjectInfo(cr), err)
		}
		klog.Infof("object changed: %s (metadata=%t, spec=%t): %s", utilObjectInfo(cr), metadataChanged, specChanged, difference)

		updatedCR, err := c.clients.RegOp.ImageregistryV1().Configs().Update(
			context.TODO(), cr, metaapi.UpdateOptions{},
		)
		if err != nil {
			if !errors.IsConflict(err) {
				klog.Errorf("unable to update %s: %s", utilObjectInfo(cr), err)
			}
			return err
		}

		// If we updated the Status field too, we'll make one more call and we
		// want it to succeed.
		cr.ResourceVersion = updatedCR.ResourceVersion
	}

	cr.Status.ObservedGeneration = cr.Generation
	statusChanged := !reflect.DeepEqual(prevCR.Status, cr.Status)
	if statusChanged {
		difference, err := object.DiffString(prevCR, cr)
		if err != nil {
			klog.Errorf("unable to calculate difference in %s: %s", utilObjectInfo(cr), err)
		}
		klog.Infof("object changed: %s (status=%t): %s", utilObjectInfo(cr), statusChanged, difference)

		_, err = c.clients.RegOp.ImageregistryV1().Configs().UpdateStatus(
			context.TODO(), cr, metaapi.UpdateOptions{},
		)
		if err != nil {
			if !errors.IsConflict(err) {
				klog.Errorf("unable to update status %s: %s", utilObjectInfo(cr), err)
			}
			return err
		}
	}

	if _, ok := applyError.(permanentError); !ok {
		return applyError
	}

	return nil
}

func (c *Controller) eventProcessor() {
	for {
		obj, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}

		klog.V(1).Infof("get event from workqueue")
		func() {
			defer c.workqueue.Done(obj)

			if _, ok := obj.(string); !ok {
				c.workqueue.Forget(obj)
				klog.Errorf("expected string in workqueue but got %#v", obj)
				return
			}

			if err := c.sync(); err != nil {
				c.workqueue.AddRateLimited(workqueueKey)
				klog.Errorf("unable to sync: %s, requeuing", err)
			} else {
				c.workqueue.Forget(obj)
				klog.Infof("event from workqueue successfully processed")
			}
		}()
	}
}

func (c *Controller) handler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(o interface{}) {
			if clusterOperator, ok := o.(*configapiv1.ClusterOperator); ok {
				if clusterOperator.GetName() != defaults.ImageRegistryClusterOperatorResourceName {
					return
				}
			}
			obj := o.(metaapi.Object)
			if obj.GetNamespace() == "kube-system" && obj.GetName() != "cluster-config-v1" {
				return
			}
			klog.V(1).Infof("add event to workqueue due to %s (add)", utilObjectInfo(o))
			c.workqueue.Add(workqueueKey)
		},
		UpdateFunc: func(o, n interface{}) {
			newAccessor, err := kmeta.Accessor(n)
			if err != nil {
				klog.Errorf("unable to get accessor for new object: %s", err)
				return
			}
			oldAccessor, err := kmeta.Accessor(o)
			if err != nil {
				klog.Errorf("unable to get accessor for old object: %s", err)
				return
			}
			if newAccessor.GetResourceVersion() == oldAccessor.GetResourceVersion() {
				// Periodic resync will send update events for all known resources.
				// Two different versions of the same resource will always have different RVs.
				return
			}
			if clusterOperator, ok := o.(*configapiv1.ClusterOperator); ok {
				if clusterOperator.GetName() != defaults.ImageRegistryClusterOperatorResourceName {
					return
				}
			}
			obj := o.(metaapi.Object)
			if obj.GetNamespace() == "kube-system" && obj.GetName() != "cluster-config-v1" {
				return
			}
			klog.V(1).Infof("add event to workqueue due to %s (update)", utilObjectInfo(n))
			c.workqueue.Add(workqueueKey)
		},
		DeleteFunc: func(o interface{}) {
			object, ok := o.(metaapi.Object)
			if !ok {
				tombstone, ok := o.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.Errorf("error decoding object, invalid type")
					return
				}
				object, ok = tombstone.Obj.(metaapi.Object)
				if !ok {
					klog.Errorf("error decoding object tombstone, invalid type")
					return
				}
				klog.V(4).Infof("recovered deleted object %q from tombstone", object.GetName())
			}
			if clusterOperator, ok := o.(*configapiv1.ClusterOperator); ok {
				if clusterOperator.GetName() != defaults.ImageRegistryClusterOperatorResourceName {
					return
				}
			}
			obj := o.(metaapi.Object)
			if obj.GetNamespace() == "kube-system" && obj.GetName() != "cluster-config-v1" {
				return
			}
			klog.V(1).Infof("add event to workqueue due to %s (delete)", utilObjectInfo(object))
			c.workqueue.Add(workqueueKey)
		},
	}
}

// Run starts the Controller.
func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer c.workqueue.ShutDown()

	var err error

	c.clients.Core, err = coreset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	c.clients.Apps, err = appsset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	c.clients.RBAC, err = rbacset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	c.clients.Kube, err = kubeset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	c.clients.Route, err = routesetv1.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	c.clients.Config, err = configsetv1.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	c.clients.RegOp, err = regopset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	c.clients.Batch, err = batchset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	routeClient, err := routeset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	configClient, err := configset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	configInformerFactory := configinformers.NewSharedInformerFactory(configClient, defaultResyncDuration)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(c.clients.Kube, defaultResyncDuration, kubeinformers.WithNamespace(defaults.ImageRegistryOperatorNamespace))
	openshiftConfigKubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(c.clients.Kube, defaultResyncDuration, kubeinformers.WithNamespace(defaults.OpenShiftConfigNamespace))
	kubeSystemKubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(c.clients.Kube, defaultResyncDuration, kubeinformers.WithNamespace(kubeSystemNamespace))
	regopInformerFactory := regopinformers.NewSharedInformerFactory(c.clients.RegOp, defaultResyncDuration)
	routeInformerFactory := routeinformers.NewSharedInformerFactoryWithOptions(routeClient, defaultResyncDuration, routeinformers.WithNamespace(defaults.ImageRegistryOperatorNamespace))

	var informers []cache.SharedIndexInformer
	for _, ctor := range []func() cache.SharedIndexInformer{
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Apps().V1().Deployments()
			c.listers.Deployments = informer.Lister().Deployments(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Apps().V1().DaemonSets()
			c.listers.DaemonSets = informer.Lister().DaemonSets(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().Services()
			c.listers.Services = informer.Lister().Services(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().Secrets()
			c.listers.Secrets = informer.Lister().Secrets(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().ConfigMaps()
			c.listers.ConfigMaps = informer.Lister().ConfigMaps(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().ServiceAccounts()
			c.listers.ServiceAccounts = informer.Lister().ServiceAccounts(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := routeInformerFactory.Route().V1().Routes()
			c.listers.Routes = informer.Lister().Routes(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Rbac().V1().ClusterRoles()
			c.listers.ClusterRoles = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Rbac().V1().ClusterRoleBindings()
			c.listers.ClusterRoleBindings = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := openshiftConfigKubeInformerFactory.Core().V1().ConfigMaps()
			c.listers.OpenShiftConfig = informer.Lister().ConfigMaps(defaults.OpenShiftConfigNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().Images()
			c.listers.ImageConfigs = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().ClusterOperators()
			c.listers.ClusterOperators = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().Proxies()
			c.listers.ProxyConfigs = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := regopInformerFactory.Imageregistry().V1().Configs()
			c.listers.RegistryConfigs = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := regopInformerFactory.Imageregistry().V1().ImagePruners()
			c.listers.ImagePrunerConfigs = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeSystemKubeInformerFactory.Core().V1().ConfigMaps()
			c.listers.InstallerConfigMaps = informer.Lister().ConfigMaps(kubeSystemNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := configInformerFactory.Config().V1().Infrastructures()
			c.listers.Infrastructures = informer.Lister()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Batch().V1beta1().CronJobs()
			c.listers.CronJobs = informer.Lister().CronJobs(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Batch().V1().Jobs()
			c.listers.Jobs = informer.Lister().Jobs(defaults.ImageRegistryOperatorNamespace)
			return informer.Informer()
		},
	} {
		informer := ctor()
		informer.AddEventHandler(c.handler())
		informers = append(informers, informer)
	}

	imageConfigStatusController := NewImageConfigController(
		c.clients.Config,
		routeInformerFactory.Route().V1().Routes(),
		kubeInformerFactory.Core().V1().Services(),
	)

	clusterOperatorStatusController := NewClusterOperatorStatusController(
		c.clients.Config,
		configInformerFactory.Config().V1().ClusterOperators(),
		regopInformerFactory.Imageregistry().V1().Configs(),
		kubeInformerFactory.Apps().V1().Deployments(),
		c.kubeconfig, c.clients, c.listers,
	)

	imageRegistryCertificatesController := NewImageRegistryCertificatesController(
		c.clients.Core,
		kubeInformerFactory.Core().V1().ConfigMaps(),
		kubeInformerFactory.Core().V1().Services(),
		configInformerFactory.Config().V1().Images(),
		openshiftConfigKubeInformerFactory.Core().V1().ConfigMaps(),
	)

	nodeCADaemonController := NewNodeCADaemonController(
		c.clients.Apps,
		kubeInformerFactory.Apps().V1().DaemonSets(),
		kubeInformerFactory.Core().V1().Services(),
	)

	configInformerFactory.Start(stopCh)
	kubeInformerFactory.Start(stopCh)
	openshiftConfigKubeInformerFactory.Start(stopCh)
	kubeSystemKubeInformerFactory.Start(stopCh)
	regopInformerFactory.Start(stopCh)
	routeInformerFactory.Start(stopCh)

	// TODO(dmage): these controllers should be started from main.
	go clusterOperatorStatusController.Run(stopCh)
	go nodeCADaemonController.Run(stopCh)
	go imageRegistryCertificatesController.Run(stopCh)
	go imageConfigStatusController.Run(stopCh)

	klog.Info("waiting for informer caches to sync")
	for _, informer := range informers {
		if ok := cache.WaitForCacheSync(stopCh, informer.HasSynced); !ok {
			return fmt.Errorf("failed to wait for caches to sync")
		}
	}

	go wait.Until(c.eventProcessor, time.Second, stopCh)

	klog.Info("started events processor")
	<-stopCh
	klog.Info("shutting down events processor")

	return nil
}
