package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	"k8s.io/kubectl/pkg/scheme"
)

const includeAnno = "sec-ctrl/replicate"
const excludeAnno = "sec-ctrl/exclude"
const dataKey = ".dockerconfigjson"
const secController = "secret-replication-controller"

const (
	SuccessSynced         = "Synced"
	SuccessSyncedTemplate = "Secret %s synced successfully"
)

type Controller struct {
	kubeclientset kubernetes.Interface

	//listers allows us to receive changes without querying all the objects from the API
	secretsLister listers.SecretLister
	nsLister      listers.NamespaceLister
	secretsSynced cache.InformerSynced

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder
}

func NewController(
	kubeclientset kubernetes.Interface,
	secretInformer coreinformers.SecretInformer,
	nsInformer coreinformers.NamespaceInformer) *Controller {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: secController})

	controller := &Controller{
		kubeclientset: kubeclientset,
		secretsLister: secretInformer.Lister(),
		secretsSynced: secretInformer.Informer().HasSynced,
		nsLister:      nsInformer.Lister(),
		workqueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Secrets"),
		recorder:      recorder,
	}

	secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newSec := new.(*corev1.Secret)
			oldSec := old.(*corev1.Secret)
			if newSec.ResourceVersion == oldSec.ResourceVersion {
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})

	return controller
}

func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		klog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	name := object.GetName()
	klog.V(4).Infof("Processing object: %s", name)
	sec, err := c.secretsLister.Secrets(object.GetNamespace()).Get(name)
	if err != nil {
		klog.V(4).Infof("ignoring object '%s", name)
		return
	}
	c.enqueueSecret(sec)
}

// Run will sync informer caches and start workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting secrets controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.secretsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

// enqueueSecret takes a Secret and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Secret.
func (c *Controller) enqueueSecret(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncSecret.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.syncSecret(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) syncSecret(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Secret resource with this namespace/name
	sec, err := c.secretsLister.Secrets(namespace).Get(name)
	if err != nil {
		// Secret may no longer exist, in which case we stop processing.
		if k8serrors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("secret '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	replicate := false
	excl := ""
	for k, val := range sec.Annotations {
		switch k {
		case includeAnno:
			if sec.Type == corev1.SecretTypeDockerConfigJson {
				replicate = true
			}
		case excludeAnno:
			excl = val

		}
	}
	if replicate {
		klog.Infof("Secret exclude list: %s", excl)
		// get the list of namespaces to exclude
		excludes := excludeList(excl)

		// get a list of namespaces to apply the secret to
		ns, err := c.nsLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("loading namespace issue: %s", err)
			return err
		}
		if ns == nil {
			return errors.New("no namespaces were found")
		}

		for _, destNs := range ns {
			// if destination namespace is not in exclude list, we apply it
			if _, ok := excludes[destNs.Name]; !ok && destNs.Name != sec.Namespace {
				klog.Infof("Replicating %s to %s", sec.Name, destNs.Name)
				c.applySecret(sec, destNs.Name)
			}
		}
		c.recorder.Event(sec, corev1.EventTypeNormal, SuccessSynced, fmt.Sprintf(SuccessSyncedTemplate, sec.Name))
	}

	return nil
}

func newSecret(orig *corev1.Secret) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: orig.Name,
		},
		Type: orig.Type,
	}
	if sec.Data == nil {
		sec.Data = make(map[string][]byte)
	}
	sec.Data[dataKey] = orig.Data[dataKey]
	return sec
}

func (c *Controller) applySecret(sec *corev1.Secret, ns string) {
	_, err := c.secretsLister.Secrets(ns).Get(sec.Name)
	// If the secret doesn't exist, we'll create it
	if k8serrors.IsNotFound(err) {
		_, err = c.kubeclientset.CoreV1().Secrets(ns).Create(newSecret(sec))
		klog.Infof("New secret was created under %s/%s", ns, sec.Name)
		return
	}
	//else we will update it
	_, err = c.kubeclientset.CoreV1().Secrets(ns).Update(newSecret(sec))
	klog.Infof("Existing secret was updated %s/%s", ns, sec.Name)
}

func excludeList(excl string) map[string]struct{} {
	klog.Info(excl)
	res := make(map[string]struct{})
	var exists = struct{}{}
	for _, item := range strings.Split(excl, ",") {

		res[item] = exists
	}
	return res
}
