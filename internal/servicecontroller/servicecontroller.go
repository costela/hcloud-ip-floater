package servicecontroller

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/costela/hcloud-ip-floater/internal/config"
	"github.com/costela/hcloud-ip-floater/internal/fipcontroller"
	"github.com/costela/hcloud-ip-floater/internal/stringset"
)

type podInformerType struct {
	factory informers.SharedInformerFactory
	stopper chan struct{}
}

type Controller struct {
	Logger logrus.FieldLogger
	K8S    *kubernetes.Clientset
	FIPc   *fipcontroller.Controller

	svcIPs   map[string]stringset.StringSet
	svcIPsMu sync.RWMutex

	svcInformerFactory informers.SharedInformerFactory
	podInformers       map[string]podInformerType
	podInformersMu     sync.RWMutex
}

func (sc *Controller) Run() {
	sc.svcInformerFactory = informers.NewSharedInformerFactoryWithOptions(
		sc.K8S,
		time.Duration(config.Global.SyncSeconds)*time.Second,
		informers.WithTweakListOptions(func(listOpts *metav1.ListOptions) {
			listOpts.LabelSelector = config.Global.ServiceLabelSelector
		}),
	)
	sc.svcIPs = make(map[string]stringset.StringSet)
	sc.podInformers = make(map[string]podInformerType)

	svcInformer := sc.svcInformerFactory.Core().V1().Services().Informer()
	stopper := make(chan struct{})
	defer close(stopper)

	svcInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(newObj interface{}) {
			newSvc, ok := newObj.(*corev1.Service)
			if !ok {
				sc.Logger.Errorf("received unexpected object type: %T", newObj)
				return
			}
			if sc.unsupportedServiceType(newSvc) {
				return
			}
			if err := sc.handleServiceAdd(newSvc); err != nil {
				sc.Logger.WithError(err).Error("error handling new service")
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldSvc, ok := oldObj.(*corev1.Service)
			if !ok {
				sc.Logger.Errorf("received unexpected old object type: %T", oldObj)
				return
			}
			newSvc, ok := newObj.(*corev1.Service)
			if !ok {
				sc.Logger.Errorf("received unexpected new object type: %T", newObj)
				return
			}
			if sc.unsupportedServiceType(newSvc) {
				return
			}
			if err := sc.handleServiceUpdate(oldSvc, newSvc); err != nil {
				sc.Logger.WithError(err).Error("error handling service update")
			}
		},
		DeleteFunc: func(oldObj interface{}) {
			oldSvc, ok := oldObj.(*corev1.Service)
			if !ok {
				sc.Logger.Errorf("received unexpected old object type: %T", oldObj)
				return
			}
			if sc.unsupportedServiceType(oldSvc) {
				return
			}
			svcKey, err := cache.MetaNamespaceKeyFunc(oldSvc)
			if err != nil {
				return
			}
			sc.forgetServiceIPs(svcKey)
			if err := sc.removePodInformer(oldSvc); err != nil {
				sc.Logger.WithError(err).Error("error removing pod informer")
			}
		},
	})

	svcInformer.Run(stopper)
}

func (sc *Controller) handleServiceAdd(svc *corev1.Service) error {
	sc.Logger.WithFields(logrus.Fields{
		"namespace": svc.Namespace,
		"service":   svc.Name,
	}).Info("new service")

	// we do not need to call handleServiceIPs here, because it will be triggered by the pod detection
	return sc.addPodInformer(svc)
}

func (sc *Controller) handleServiceUpdate(oldSvc, newSvc *corev1.Service) error {
	sc.Logger.WithFields(logrus.Fields{
		"namespace": newSvc.Namespace,
		"service":   newSvc.Name,
	}).Info("service update")

	oldIPs := getLoadbalancerIPs(oldSvc)
	newIPs := getLoadbalancerIPs(newSvc)

	if len(oldIPs) != len(newIPs) {
		return sc.handleServiceIPs(newSvc, newIPs)
	}

	for i := range oldIPs {
		if !newIPs.Has(i) {
			return sc.handleServiceIPs(newSvc, newIPs)
		}
	}

	if labels.Set(oldSvc.Spec.Selector).String() != labels.Set(newSvc.Spec.Selector).String() {
		return sc.replacePodInformer(oldSvc, newSvc)
	}

	sc.Logger.WithFields(logrus.Fields{
		"namespace": newSvc.Namespace,
		"service":   newSvc.Name,
	}).Info("service unchanged")

	return nil
}

func (sc *Controller) addPodInformer(svc *corev1.Service) error {
	sc.Logger.WithFields(logrus.Fields{
		"namespace": svc.Namespace,
		"service":   svc.Name,
	}).Info("adding pod informer")

	svcKey, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return err
	}

	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		sc.K8S,
		time.Duration(config.Global.SyncSeconds)*time.Second,
		informers.WithNamespace(svc.Namespace),
		informers.WithTweakListOptions(func(listOpts *metav1.ListOptions) {
			listOpts.LabelSelector = labels.Set(svc.Spec.Selector).String()
		}),
	)
	podInformer := podInformerFactory.Core().V1().Pods().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// covers newly discovered (but already "ready") pods
		AddFunc: func(newObj interface{}) {
			newPod, ok := newObj.(*corev1.Pod)
			if !ok {
				sc.Logger.Errorf("received unexpected object type: %T", newObj)
				return
			}
			if err := sc.handleNewPod(svcKey, newPod); err != nil {
				sc.Logger.WithError(err).Error("could not handle new pod")
			}
		},
		// covers pods becoming ready/not-ready
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, ok := oldObj.(*corev1.Pod)
			if !ok {
				sc.Logger.Errorf("received unexpected object type: %T", oldObj)
				return
			}
			newPod, ok := newObj.(*corev1.Pod)
			if !ok {
				sc.Logger.Errorf("received unexpected object type: %T", newObj)
				return
			}

			if err := sc.handlePodUpdate(svcKey, oldPod, newPod); err != nil {
				sc.Logger.WithError(err).Error("could not handle pod update")
			}
		},
	})

	stopper := make(chan struct{})

	sc.podInformersMu.Lock()
	defer sc.podInformersMu.Unlock()

	sc.podInformers[svcKey] = podInformerType{
		factory: podInformerFactory,
		stopper: stopper,
	}

	go podInformer.Run(stopper)

	return nil
}

func (sc *Controller) removePodInformer(svc *corev1.Service) error {
	svcKey, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return err
	}

	sc.podInformersMu.Lock()
	defer sc.podInformersMu.Unlock()

	podInformer, ok := sc.podInformers[svcKey]
	if !ok {
		return nil // ignore for now
	}

	sc.Logger.WithFields(logrus.Fields{
		"namespace": svc.Namespace,
		"service":   svc.Name,
	}).Info("removing pod informer")

	delete(sc.podInformers, svcKey)
	close(podInformer.stopper)

	return nil
}

func (sc *Controller) replacePodInformer(oldSvc, newSvc *corev1.Service) error {
	// TODO: too simple: we might miss events between remove/add; should fetch old/replace/close
	if err := sc.removePodInformer(oldSvc); err != nil {
		return err
	}

	if err := sc.addPodInformer(newSvc); err != nil {
		return err
	}

	return nil
}

func (sc *Controller) handleNewPod(svcKey string, newPod *corev1.Pod) error {
	svc, err := sc.getServiceFromKey(svcKey)
	if err != nil {
		return err
	}

	funcLogger := sc.Logger.WithFields(logrus.Fields{
		"namespace": svc.Namespace,
		"service":   svc.Name,
		"pod":       newPod.Name,
	})

	if !podIsReady(newPod) {
		funcLogger.Debug("ignoring non-ready pod")
		return nil
	}

	ips := getLoadbalancerIPs(svc)
	return sc.handleServiceIPs(svc, ips)
}

func (sc *Controller) handlePodUpdate(svcKey string, oldPod, newPod *corev1.Pod) error {
	svc, err := sc.getServiceFromKey(svcKey)
	if err != nil {
		return err
	}

	funcLogger := sc.Logger.WithFields(logrus.Fields{
		"namespace": svc.Namespace,
		"service":   svc.Name,
		"pod":       newPod.Name,
	})

	oldReady := podIsReady(oldPod)
	newReady := podIsReady(newPod)

	if oldReady == newReady {
		funcLogger.Debug("pod readiness unchanged")
		return nil
	} else if oldReady {
		funcLogger.Info("pod became not-ready")
	} else if newReady {
		funcLogger.Info("pod became ready")
	} else {
		return nil // some other uninteresting state transition
	}

	ips := getLoadbalancerIPs(svc)
	return sc.handleServiceIPs(svc, ips)
}

func (sc *Controller) getServiceFromKey(svcKey string) (*corev1.Service, error) {
	obj, _, err := sc.svcInformerFactory.Core().V1().Services().Informer().GetIndexer().GetByKey(svcKey)
	if err != nil {
		return nil, fmt.Errorf("could not find service %s: %w", svcKey, err)
	}

	svc, ok := obj.(*corev1.Service)
	if !ok {
		return nil, fmt.Errorf("got unexpected obj type %T", obj)
	}

	return svc, nil
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (sc *Controller) handleServiceIPs(svc *corev1.Service, svcIPs stringset.StringSet) error {
	// TODO: use util/workqueue to avoid blocking informer if hcloud API is slow

	svcKey, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return err
	}

	sc.updateServiceIPs(svcKey, svcIPs)

	if len(svcIPs) == 0 {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
		}).Info("service has no IPs")
		return nil
	}

	nodes, err := sc.getServiceReadyNodes(svcKey)
	if err != nil {
		return err
	}

	if len(nodes) == 0 {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
		}).Info("service has no ready pods")
		return nil
	}

	// Order ready nodes by hash of node#service, the same way MetalLB does
	// This means we will pick the same node MetalLB does so services with externalTrafficPolicy=Local work correctly
	sort.Slice(nodes, func(i, j int) bool {
		hi := sha256.Sum256([]byte(nodes[i] + "#" + svcKey))
		hj := sha256.Sum256([]byte(nodes[j] + "#" + svcKey))

		return bytes.Compare(hi[:], hj[:]) < 0
	})

	electedNode := nodes[0]

	sc.FIPc.AttachToNode(svcIPs, electedNode)
	return nil
}

// getServiceReadyNodes gets all nodes where ready pods are scheduled
func (sc *Controller) getServiceReadyNodes(svcKey string) ([]string, error) {
	sc.podInformersMu.RLock()
	podInformerFactory, ok := sc.podInformers[svcKey]
	sc.podInformersMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("could not find informer factory for svc %s", svcKey)
	}

	// LabelSelector comes from the podInformerFactory
	pods, err := podInformerFactory.factory.Core().V1().Pods().Lister().List(labels.NewSelector())
	if err != nil {
		return nil, err
	}

	nodes := make([]string, 0, len(pods))
	for _, pod := range pods {
		if podIsReady(pod) {
			nodes = append(nodes, pod.Spec.NodeName)
		}
	}

	return nodes, nil
}

func (sc *Controller) unsupportedServiceType(svc *corev1.Service) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
		}).Info("skipping non-LoadBalancer service")

		return true
	}
	return false
}

func (sc *Controller) updateServiceIPs(svcKey string, svcIPs stringset.StringSet) {
	sc.svcIPsMu.Lock()
	defer sc.svcIPsMu.Unlock()

	oldIPs := sc.svcIPs[svcKey]
	sc.FIPc.ForgetAttachments(oldIPs.Diff(svcIPs))
}

func (sc *Controller) forgetServiceIPs(svcKey string) {
	sc.svcIPsMu.Lock()
	defer sc.svcIPsMu.Unlock()

	sc.FIPc.ForgetAttachments(sc.svcIPs[svcKey])

	delete(sc.svcIPs, svcKey)
}

func getLoadbalancerIPs(svc *corev1.Service) stringset.StringSet {
	ips := make(stringset.StringSet, len(svc.Status.LoadBalancer.Ingress))

	// ignore svc.Spec.LoadBalancerIP; it's provided as a request and may be ignored by k8s

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			ips.Add(ingress.IP)
		}
	}
	return ips
}
