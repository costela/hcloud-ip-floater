package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/costela/hcloud-ip-floater/internal/config"
)

type podInformerType struct {
	factory informers.SharedInformerFactory
	stopper chan struct{}
}

type ServiceController struct {
	Logger       logrus.FieldLogger
	K8S          *kubernetes.Clientset
	HCloudClient *hcloud.Client

	sf             singleflight.Group
	svcInformer    informers.SharedInformerFactory
	podInformers   map[string]podInformerType
	podInformersMu sync.RWMutex
}

func (sc *ServiceController) Run() {
	sc.svcInformer = informers.NewSharedInformerFactoryWithOptions(
		sc.K8S,
		5*time.Minute,
		informers.WithTweakListOptions(func(listOpts *metav1.ListOptions) {
			listOpts.LabelSelector = config.Global.ServiceLabelSelector
		}),
	)
	sc.podInformers = make(map[string]podInformerType)

	svcInformer := sc.svcInformer.Core().V1().Services().Informer()
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
			if err := sc.removePodInformer(oldSvc); err != nil {
				sc.Logger.WithError(err).Error("error removing pod informer")
			}
		},
	})

	svcInformer.Run(stopper)
}

func (sc *ServiceController) handleServiceAdd(svc *corev1.Service) error {
	sc.Logger.WithFields(logrus.Fields{
		"namespace": svc.Namespace,
		"service":   svc.Name,
	}).Info("new service")

	// we do not need to call handleServiceIPs here, because it will be triggered by the pod detection
	return sc.addPodInformer(svc)
}

func (sc *ServiceController) handleServiceUpdate(oldSvc, newSvc *corev1.Service) error {
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
		if oldIPs[i] != newIPs[i] {
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

func (sc *ServiceController) addPodInformer(svc *corev1.Service) error {
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
		5*time.Minute,
		informers.WithTweakListOptions(func(listOpts *metav1.ListOptions) {
			listOpts.LabelSelector = labels.Set(svc.Spec.Selector).String()
		}),
	)
	podInformer := podInformerFactory.Core().V1().Pods().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// we ignore Add/Delete because we're only interested in state changes (to and from "ready")
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

func (sc *ServiceController) removePodInformer(svc *corev1.Service) error {
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

func (sc *ServiceController) replacePodInformer(oldSvc, newSvc *corev1.Service) error {
	// TODO: too simple: we might miss events between remove/add; should fetch old/replace/close
	if err := sc.removePodInformer(oldSvc); err != nil {
		return err
	}

	if err := sc.addPodInformer(newSvc); err != nil {
		return err
	}

	return nil
}

func (sc *ServiceController) handlePodUpdate(svcKey string, oldPod, newPod *corev1.Pod) error {
	obj, _, err := sc.svcInformer.Core().V1().Services().Informer().GetIndexer().GetByKey(svcKey)
	if err != nil {
		return err
	}

	svc, ok := obj.(*corev1.Service)
	if !ok {
		return fmt.Errorf("got unexpected obj type %T", obj)
	}

	if podIsReady(oldPod) == podIsReady(newPod) {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
			"pod":       newPod.Name,
		}).Debug("pod readiness unchanged")
		return nil
	}

	if podIsReady(oldPod) {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
			"pod":       oldPod.Name,
		}).Info("pod became not-ready")
	} else if podIsReady(newPod) {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
			"pod":       newPod.Name,
		}).Info("pod became ready")
	} else {
		return nil // some other uninteresting state transition
	}

	ips := getLoadbalancerIPs(svc)
	return sc.handleServiceIPs(svc, ips)
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (sc *ServiceController) handleServiceIPs(svc *corev1.Service, svcIPs []string) error {
	// TODO: use util/workqueue to avoid blocking informer if hcloud API is slow
	fipAllocation, err := sc.getFIPAllocations()
	if err != nil {
		return err
	}

	usableFIPs := make([]*hcloud.FloatingIP, 0)

	for _, ip := range svcIPs {
		if fip, ok := fipAllocation[ip]; ok {
			usableFIPs = append(usableFIPs, fip)
		}
	}

	if len(usableFIPs) == 0 {
		return fmt.Errorf("service IPs %s not found in floating IP list", svcIPs)
	}

	nodes, err := sc.getServiceReadyNodes(svc)
	if err != nil {
		return err
	}

	if len(nodes) == 0 {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
		}).Info("ignoring service with no ready pods")
		return nil
	}

	// TODO: this is probably too simple, but should at least be deterministic
	electedNode := nodes[0]

	for _, fip := range usableFIPs {
		if electedNode == fip.Server.Name {
			sc.Logger.WithFields(logrus.Fields{
				"fip":       fip.IP,
				"namespace": svc.Namespace,
				"service":   svc.Name,
				"node":      electedNode,
			}).Info("floating IP already attached")
			continue
		}

		// TODO: use multierr?
		if err := sc.attachFIPToNode(fip, electedNode); err != nil {
			sc.Logger.WithError(err).WithFields(logrus.Fields{
				"fip":       fip.IP,
				"namespace": svc.Namespace,
				"service":   svc.Name,
				"node":      electedNode,
			}).Errorf("could not attach floating IP")
		}
		sc.Logger.WithFields(logrus.Fields{
			"fip":       fip.IP,
			"namespace": svc.Namespace,
			"service":   svc.Name,
			"node":      electedNode,
		}).Info("floating IP attached")
	}

	return nil
}

// getServiceReadyNodes gets all nodes where ready pods are scheduled
func (sc *ServiceController) getServiceReadyNodes(svc *corev1.Service) ([]string, error) {
	svcKey, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return nil, err
	}

	sc.podInformersMu.RLock()
	podInformerFactory, ok := sc.podInformers[svcKey]
	sc.podInformersMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("could not find informer factory for svc %s", svcKey)
	}

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

func (sc *ServiceController) attachFIPToNode(fip *hcloud.FloatingIP, node string) error {
	server, _, err := sc.HCloudClient.Server.GetByName(context.Background(), node)
	if err != nil {
		return err
	}
	act, _, err := sc.HCloudClient.FloatingIP.Assign(context.Background(), fip, server)
	if err != nil {
		return err
	}
	_, errc := sc.HCloudClient.Action.WatchProgress(context.Background(), act)
	return <-errc
}

type fipAllocation map[string]*hcloud.FloatingIP

func (sc *ServiceController) getFIPAllocations() (fipAllocation, error) {
	res, err, _ := sc.sf.Do("", func() (interface{}, error) {
		fips, err := sc.HCloudClient.FloatingIP.AllWithOpts(context.Background(), hcloud.FloatingIPListOpts{
			ListOpts: hcloud.ListOpts{
				LabelSelector: config.Global.FloatingLabelSelector,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("could not fetch floating IPs: %w", err)
		}
		ips := make(fipAllocation, len(fips))
		for _, fip := range fips {
			// TODO: cache server info
			srv, _, err := sc.HCloudClient.Server.GetByID(context.Background(), fip.Server.ID)
			if err != nil {
				return nil, fmt.Errorf("could not resolve server name for %d: %w", fip.Server.ID, err)
			}
			fip.Server = srv
			ips[fip.IP.String()] = fip
		}

		return ips, nil
	})
	return res.(fipAllocation), err
}

func (sc *ServiceController) unsupportedServiceType(svc *corev1.Service) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		sc.Logger.WithFields(logrus.Fields{
			"namespace": svc.Namespace,
			"service":   svc.Name,
		}).Info("skipping non-LoadBalancer service")

		return true
	}
	return false
}

func getLoadbalancerIPs(svc *corev1.Service) []string {
	ips := make([]string, 0, len(svc.Status.LoadBalancer.Ingress))

	// ignore svc.Spec.LoadBalancerIP; it's provided as a request and may be ignored by k8s

	for _, ip := range svc.Status.LoadBalancer.Ingress {
		if ip.IP != "" {
			ips = append(ips, ip.IP)
		}
	}
	return ips
}
