package factory

import (
	"fmt"
	"hash/fnv"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Handler represents an event handler and is private to the factory module
type Handler struct {
	base cache.FilteringResourceEventHandler

	id uint64
	// tombstone is used to track the handler's lifetime. handlerAlive
	// indicates the handler can be called, while handlerDead indicates
	// it has been scheduled for removal and should not be called.
	// tombstone should only be set using atomic operations since it is
	// used from multiple goroutines.
	tombstone uint32
}

func (h *Handler) OnAdd(obj interface{}) {
	if atomic.LoadUint32(&h.tombstone) == handlerAlive {
		h.base.OnAdd(obj)
	}
}

func (h *Handler) OnUpdate(oldObj, newObj interface{}) {
	if atomic.LoadUint32(&h.tombstone) == handlerAlive {
		h.base.OnUpdate(oldObj, newObj)
	}
}

func (h *Handler) OnDelete(obj interface{}) {
	if atomic.LoadUint32(&h.tombstone) == handlerAlive {
		h.base.OnDelete(obj)
	}
}

func (h *Handler) kill() error {
	if !atomic.CompareAndSwapUint32(&h.tombstone, handlerAlive, handlerDead) {
		return fmt.Errorf("event handler %d already dead", h.id)
	}
	return nil
}

type eventKind int

const (
	addEvent eventKind = iota
	updateEvent
	deleteEvent
)

type event struct {
	obj    interface{}
	oldObj interface{}
	kind   eventKind
}

type informer struct {
	sync.RWMutex
	oType    reflect.Type
	inf      cache.SharedIndexInformer
	handlers map[uint64]*Handler
	events   []chan *event
}

func (i *informer) forEachQueuedHandler(f func(h *Handler)) {
	i.RLock()
	curHandlers := make([]*Handler, 0, len(i.handlers))
	for _, handler := range i.handlers {
		curHandlers = append(curHandlers, handler)
	}
	i.RUnlock()

	for _, handler := range curHandlers {
		f(handler)
	}
}

func (i *informer) forEachHandler(obj interface{}, f func(h *Handler)) {
	i.Lock()
	defer i.Unlock()

	objType := reflect.TypeOf(obj)
	if objType != i.oType {
		logrus.Errorf("object type %v did not match expected %v", objType, i.oType)
		return
	}

	for _, handler := range i.handlers {
		f(handler)
	}
}

func (i *informer) addHandler(id uint64, filterFunc func(obj interface{}) bool, funcs cache.ResourceEventHandler) *Handler {
	i.Lock()
	defer i.Unlock()

	handler := &Handler{
		cache.FilteringResourceEventHandler{
			FilterFunc: filterFunc,
			Handler:    funcs,
		},
		id,
		handlerAlive,
	}
	i.handlers[id] = handler
	return handler
}

func (i *informer) removeHandler(handler *Handler) error {
	if err := handler.kill(); err != nil {
		return err
	}

	logrus.Debugf("sending %v event handler %d for removal", i.oType, handler.id)

	go func() {
		i.Lock()
		defer i.Unlock()
		if _, ok := i.handlers[handler.id]; ok {
			// Remove the handler
			delete(i.handlers, handler.id)
			logrus.Debugf("removed %v event handler %d", i.oType, handler.id)
		} else {
			logrus.Warningf("tried to remove unknown object type %v event handler %d", i.oType, handler.id)
		}
	}()

	return nil
}

func (i *informer) processEvents(events chan *event, stopChan <-chan struct{}) {
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return
			}
			switch e.kind {
			case addEvent:
				i.forEachQueuedHandler(func(h *Handler) {
					h.OnAdd(e.obj)
				})
			case updateEvent:
				i.forEachQueuedHandler(func(h *Handler) {
					h.OnUpdate(e.oldObj, e.obj)
				})
			case deleteEvent:
				i.forEachQueuedHandler(func(h *Handler) {
					h.OnDelete(e.obj)
				})
			}
		case <-stopChan:
			return
		}
	}
}

func (i *informer) enqueueEvent(oldObj, obj interface{}, kind eventKind) {
	meta, err := getObjectMeta(i.oType, obj)
	if err != nil {
		logrus.Errorf("object has no meta: %v", err)
		return
	}

	// Distribute the object to an event queue based on a hash of its
	// namespaced name, so that all events for a given object are
	// serialized in one queue.
	h := fnv.New32()
	if meta.Namespace != "" {
		_, _ = h.Write([]byte(meta.Namespace))
		_, _ = h.Write([]byte("/"))
	}
	_, _ = h.Write([]byte(meta.Name))
	queueIdx := h.Sum32() % uint32(numEventQueues)

	i.RLock()
	defer i.RUnlock()
	if i.events[queueIdx] != nil {
		i.events[queueIdx] <- &event{
			obj:    obj,
			oldObj: oldObj,
			kind:   kind,
		}
	}
}

func ensureObjectOnDelete(obj interface{}, expectedType reflect.Type) (interface{}, error) {
	if expectedType == reflect.TypeOf(obj) {
		return obj, nil
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, fmt.Errorf("couldn't get object from tombstone: %+v", obj)
	}
	obj = tombstone.Obj
	objType := reflect.TypeOf(obj)
	if expectedType != objType {
		return nil, fmt.Errorf("expected tombstone object resource type %v but got %v", expectedType, objType)
	}
	return obj, nil
}

func (i *informer) newFederatedQueuedHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			i.enqueueEvent(nil, obj, addEvent)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			i.enqueueEvent(oldObj, newObj, updateEvent)
		},
		DeleteFunc: func(obj interface{}) {
			realObj, err := ensureObjectOnDelete(obj, i.oType)
			if err != nil {
				logrus.Errorf(err.Error())
				return
			}
			i.enqueueEvent(nil, realObj, deleteEvent)
		},
	}
}

func (i *informer) newFederatedHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			i.forEachHandler(obj, func(h *Handler) {
				h.OnAdd(obj)
			})
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			i.forEachHandler(newObj, func(h *Handler) {
				h.OnUpdate(oldObj, newObj)
			})
		},
		DeleteFunc: func(obj interface{}) {
			realObj, err := ensureObjectOnDelete(obj, i.oType)
			if err != nil {
				logrus.Errorf(err.Error())
				return
			}
			i.forEachHandler(realObj, func(h *Handler) {
				h.OnDelete(realObj)
			})
		},
	}
}

func (i *informer) shutdown() {
	i.Lock()
	defer i.Unlock()

	for _, handler := range i.handlers {
		_ = i.removeHandler(handler)
	}

	// Close all event channels for queued informers
	for idx := range i.events {
		close(i.events[idx])
		i.events[idx] = nil
	}
}

func newBaseInformer(oType reflect.Type, sharedInformer cache.SharedIndexInformer) *informer {
	return &informer{
		oType:    oType,
		inf:      sharedInformer,
		handlers: make(map[uint64]*Handler),
	}
}

func newInformer(oType reflect.Type, sharedInformer cache.SharedIndexInformer) *informer {
	i := newBaseInformer(oType, sharedInformer)
	i.inf.AddEventHandler(i.newFederatedHandler())
	return i
}

func newQueuedInformer(oType reflect.Type, sharedInformer cache.SharedIndexInformer, stopChan chan struct{}) *informer {
	i := newBaseInformer(oType, sharedInformer)
	i.events = make([]chan *event, numEventQueues)
	for j := range i.events {
		i.events[j] = make(chan *event, 1)
		go i.processEvents(i.events[j], stopChan)
	}
	i.inf.AddEventHandler(i.newFederatedQueuedHandler())
	return i
}

// WatchFactory initializes and manages common kube watches
type WatchFactory struct {
	// Must be first member in the struct due to Golang ARM/x86 32-bit
	// requirements with atomic accesses
	handlerCounter uint64

	iFactory  informerfactory.SharedInformerFactory
	informers map[reflect.Type]*informer
}

const (
	resyncInterval        = 12 * time.Hour
	handlerAlive   uint32 = 0
	handlerDead    uint32 = 1
	numEventQueues int    = 10
)

var (
	podType       reflect.Type = reflect.TypeOf(&kapi.Pod{})
	serviceType   reflect.Type = reflect.TypeOf(&kapi.Service{})
	endpointsType reflect.Type = reflect.TypeOf(&kapi.Endpoints{})
	policyType    reflect.Type = reflect.TypeOf(&knet.NetworkPolicy{})
	namespaceType reflect.Type = reflect.TypeOf(&kapi.Namespace{})
	nodeType      reflect.Type = reflect.TypeOf(&kapi.Node{})
)

// NewWatchFactory initializes a new watch factory
func NewWatchFactory(c kubernetes.Interface, stopChan chan struct{}) (*WatchFactory, error) {
	// resync time is 12 hours, none of the resources being watched in ovn-kubernetes have
	// any race condition where a resync may be required e.g. cni executable on node watching for
	// events on pods and assuming that an 'ADD' event will contain the annotations put in by
	// ovnkube master (currently, it is just a 'get' loop)
	// the downside of making it tight (like 10 minutes) is needless spinning on all resources
	wf := &WatchFactory{
		iFactory:  informerfactory.NewSharedInformerFactory(c, resyncInterval),
		informers: make(map[reflect.Type]*informer),
	}

	// Create shared informers we know we'll use
	wf.informers[podType] = newInformer(podType, wf.iFactory.Core().V1().Pods().Informer())
	wf.informers[serviceType] = newInformer(serviceType, wf.iFactory.Core().V1().Services().Informer())
	wf.informers[endpointsType] = newInformer(endpointsType, wf.iFactory.Core().V1().Endpoints().Informer())
	wf.informers[policyType] = newInformer(policyType, wf.iFactory.Networking().V1().NetworkPolicies().Informer())
	wf.informers[namespaceType] = newInformer(namespaceType, wf.iFactory.Core().V1().Namespaces().Informer())
	wf.informers[nodeType] = newQueuedInformer(nodeType, wf.iFactory.Core().V1().Nodes().Informer(), stopChan)

	wf.iFactory.Start(stopChan)
	for oType, synced := range wf.iFactory.WaitForCacheSync(stopChan) {
		if !synced {
			return nil, fmt.Errorf("error in syncing cache for %v informer", oType)
		}
	}

	go func() {
		<-stopChan

		// Remove all informer handlers
		for _, inf := range wf.informers {
			inf.shutdown()
		}
	}()

	return wf, nil
}

func getObjectMeta(objType reflect.Type, obj interface{}) (*metav1.ObjectMeta, error) {
	switch objType {
	case podType:
		if pod, ok := obj.(*kapi.Pod); ok {
			return &pod.ObjectMeta, nil
		}
	case serviceType:
		if service, ok := obj.(*kapi.Service); ok {
			return &service.ObjectMeta, nil
		}
	case endpointsType:
		if endpoints, ok := obj.(*kapi.Endpoints); ok {
			return &endpoints.ObjectMeta, nil
		}
	case policyType:
		if policy, ok := obj.(*knet.NetworkPolicy); ok {
			return &policy.ObjectMeta, nil
		}
	case namespaceType:
		if namespace, ok := obj.(*kapi.Namespace); ok {
			return &namespace.ObjectMeta, nil
		}
	case nodeType:
		if node, ok := obj.(*kapi.Node); ok {
			return &node.ObjectMeta, nil
		}
	}
	return nil, fmt.Errorf("cannot get ObjectMeta from type %v", objType)
}

func (wf *WatchFactory) addHandler(objType reflect.Type, namespace string, lsel *metav1.LabelSelector, funcs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	inf, ok := wf.informers[objType]
	if !ok {
		return nil, fmt.Errorf("unknown object type %v", objType)
	}

	sel, err := metav1.LabelSelectorAsSelector(lsel)
	if err != nil {
		return nil, fmt.Errorf("error creating label selector: %v", err)
	}

	filterFunc := func(obj interface{}) bool {
		if namespace == "" && lsel == nil {
			// Unfiltered handler
			return true
		}
		meta, err := getObjectMeta(objType, obj)
		if err != nil {
			logrus.Errorf("watch handler filter error: %v", err)
			return false
		}
		if namespace != "" && meta.Namespace != namespace {
			return false
		}
		if lsel != nil && !sel.Matches(labels.Set(meta.Labels)) {
			return false
		}
		return true
	}

	// Process existing items as a set so the caller can clean up
	// after a restart or whatever
	existingItems := inf.inf.GetStore().List()
	if processExisting != nil {
		items := make([]interface{}, 0)
		for _, obj := range existingItems {
			if filterFunc(obj) {
				items = append(items, obj)
			}
		}
		processExisting(items)
	}

	handlerID := atomic.AddUint64(&wf.handlerCounter, 1)
	handler := inf.addHandler(handlerID, filterFunc, funcs)
	logrus.Debugf("added %v event handler %d", objType, handler.id)

	// Send existing items to the handler's add function; informers usually
	// do this but since we share informers, it's long-since happened so
	// we must emulate that here
	for _, obj := range existingItems {
		handler.OnAdd(obj)
	}

	return handler, nil
}

func (wf *WatchFactory) removeHandler(objType reflect.Type, handler *Handler) error {
	if inf, ok := wf.informers[objType]; ok {
		return inf.removeHandler(handler)
	}
	return fmt.Errorf("tried to remove unknown object type %v event handler", objType)
}

// AddPodHandler adds a handler function that will be executed on Pod object changes
func (wf *WatchFactory) AddPodHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(podType, "", nil, handlerFuncs, processExisting)
}

// AddFilteredPodHandler adds a handler function that will be executed when Pod objects that match the given filters change
func (wf *WatchFactory) AddFilteredPodHandler(namespace string, lsel *metav1.LabelSelector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(podType, namespace, lsel, handlerFuncs, processExisting)
}

// RemovePodHandler removes a Pod object event handler function
func (wf *WatchFactory) RemovePodHandler(handler *Handler) error {
	return wf.removeHandler(podType, handler)
}

// AddServiceHandler adds a handler function that will be executed on Service object changes
func (wf *WatchFactory) AddServiceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(serviceType, "", nil, handlerFuncs, processExisting)
}

// RemoveServiceHandler removes a Service object event handler function
func (wf *WatchFactory) RemoveServiceHandler(handler *Handler) error {
	return wf.removeHandler(serviceType, handler)
}

// AddEndpointsHandler adds a handler function that will be executed on Endpoints object changes
func (wf *WatchFactory) AddEndpointsHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(endpointsType, "", nil, handlerFuncs, processExisting)
}

// AddFilteredEndpointsHandler adds a handler function that will be executed when Endpoint objects that match the given filters change
func (wf *WatchFactory) AddFilteredEndpointsHandler(namespace string, lsel *metav1.LabelSelector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(endpointsType, namespace, lsel, handlerFuncs, processExisting)
}

// RemoveEndpointsHandler removes a Endpoints object event handler function
func (wf *WatchFactory) RemoveEndpointsHandler(handler *Handler) error {
	return wf.removeHandler(endpointsType, handler)
}

// AddPolicyHandler adds a handler function that will be executed on NetworkPolicy object changes
func (wf *WatchFactory) AddPolicyHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(policyType, "", nil, handlerFuncs, processExisting)
}

// RemovePolicyHandler removes a NetworkPolicy object event handler function
func (wf *WatchFactory) RemovePolicyHandler(handler *Handler) error {
	return wf.removeHandler(policyType, handler)
}

// AddNamespaceHandler adds a handler function that will be executed on Namespace object changes
func (wf *WatchFactory) AddNamespaceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(namespaceType, "", nil, handlerFuncs, processExisting)
}

// AddFilteredNamespaceHandler adds a handler function that will be executed when Namespace objects that match the given filters change
func (wf *WatchFactory) AddFilteredNamespaceHandler(namespace string, lsel *metav1.LabelSelector, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(namespaceType, namespace, lsel, handlerFuncs, processExisting)
}

// RemoveNamespaceHandler removes a Namespace object event handler function
func (wf *WatchFactory) RemoveNamespaceHandler(handler *Handler) error {
	return wf.removeHandler(namespaceType, handler)
}

// AddNodeHandler adds a handler function that will be executed on Node object changes
func (wf *WatchFactory) AddNodeHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) (*Handler, error) {
	return wf.addHandler(nodeType, "", nil, handlerFuncs, processExisting)
}

// RemoveNodeHandler removes a Node object event handler function
func (wf *WatchFactory) RemoveNodeHandler(handler *Handler) error {
	return wf.removeHandler(nodeType, handler)
}
