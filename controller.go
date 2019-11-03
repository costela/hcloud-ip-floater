package main

import (
	"context"
	"fmt"
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
)

type serviceController struct {
	logger logrus.FieldLogger
	k8s    *kubernetes.Clientset
	hcc    *hcloud.Client
	sf     singleflight.Group
}

func (sc *serviceController) run() {
	factory := informers.NewSharedInformerFactoryWithOptions(
		sc.k8s,
		5*time.Minute,
		informers.WithTweakListOptions(func(listOpts *metav1.ListOptions) {
			listOpts.LabelSelector = config.ServiceLabelSelector
		}),
	)
	informer := factory.Core().V1().Services().Informer()
	stopper := make(chan struct{})
	defer close(stopper)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(newObj interface{}) {
			newSvc, ok := newObj.(*corev1.Service)
			if !ok {
				sc.logger.Errorf("received unexpected object type: %T", newObj)
			}
			if err := sc.handleServiceAdd(newSvc); err != nil {
				sc.logger.WithError(err).Error("could not handle new service")
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldSvc, ok := oldObj.(*corev1.Service)
			if !ok {
				sc.logger.Errorf("received unexpected old object type: %T", oldObj)
			}
			newSvc, ok := newObj.(*corev1.Service)
			if !ok {
				sc.logger.Errorf("received unexpected new object type: %T", newObj)
			}
			if err := sc.handleServiceUpdate(oldSvc, newSvc); err != nil {
				sc.logger.WithError(err).Error("could not handle service update")
			}
		},
	})

	informer.Run(stopper)
}

func (sc *serviceController) handleServiceAdd(svc *corev1.Service) error {
	sc.logger.WithFields(logrus.Fields{
		"service": svc.Name,
	}).Info("new service")

	return sc.handleServiceIPs(svc, getLoadbalancerIPs(svc))
}

func (sc *serviceController) handleServiceUpdate(oldSvc, newSvc *corev1.Service) error {
	sc.logger.WithFields(logrus.Fields{
		"service": newSvc.Name,
	}).Info("service update")

	// oldIPs := getLoadbalancerIPs(oldSvc)
	newIPs := getLoadbalancerIPs(newSvc)

	// if len(oldIPs) != len(newIPs) {
	// 	return sc.handleServiceIPs(newSvc, newIPs)
	// }

	// for i := range oldIPs {
	// 	if oldIPs[i] != newIPs[i] {
	// 		return sc.handleServiceIPs(newSvc, newIPs)
	// 	}
	// }

	// sc.logger.WithFields(logrus.Fields{
	// 	"service": newSvc.Name,
	// }).Info("service unchanged")

	return sc.handleServiceIPs(newSvc, newIPs)
}

func (sc *serviceController) handleServiceIPs(svc *corev1.Service, svcIPs []string) error {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		sc.logger.WithFields(logrus.Fields{
			"service": svc.Name,
		}).Info("skipping non-LoadBalancer service")

		return nil
	}

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

	nodes, err := sc.getServiceNodes(svc)
	if err != nil {
		return err
	}

	if len(nodes) == 0 {
		return fmt.Errorf("service %s has no pods", svc.Name)
	}

	// TODO: this is probably too simple, but should at least be deterministic
	electedNode := nodes[0]

	for _, fip := range usableFIPs {
		if electedNode == fip.Server.Name {
			sc.logger.WithFields(logrus.Fields{
				"fip":     fip.IP,
				"service": svc.Name,
				"node":    electedNode,
			}).Info("floating IP already attached")
			continue
		}

		// TODO: use multierr?
		if err := sc.attachFIPToNode(fip, electedNode); err != nil {
			sc.logger.WithError(err).WithFields(logrus.Fields{
				"fip":     fip.IP,
				"service": svc.Name,
				"node":    electedNode,
			}).Errorf("could not attach floating IP")
		}
		sc.logger.WithFields(logrus.Fields{
			"fip":     fip.IP,
			"service": svc.Name,
			"node":    electedNode,
		}).Info("floating IP attached")
	}

	return nil
}

func (sc *serviceController) getServiceNodes(svc *corev1.Service) ([]string, error) {
	pods, err := sc.k8s.CoreV1().Pods("").List(metav1.ListOptions{
		LabelSelector: labels.Set(svc.Spec.Selector).String(),
	})
	if err != nil {
		return nil, err
	}

	nodes := make([]string, len(pods.Items))
	for i, pod := range pods.Items {
		nodes[i] = pod.Spec.NodeName
	}

	return nodes, nil
}

func (sc *serviceController) attachFIPToNode(fip *hcloud.FloatingIP, node string) error {
	server, _, err := sc.hcc.Server.GetByName(context.Background(), node)
	if err != nil {
		return err
	}
	act, _, err := sc.hcc.FloatingIP.Assign(context.Background(), fip, server)
	if err != nil {
		return err
	}
	_, errc := sc.hcc.Action.WatchProgress(context.Background(), act)
	return <-errc
}

type fipAllocation map[string]*hcloud.FloatingIP

func (sc *serviceController) getFIPAllocations() (fipAllocation, error) {
	res, err, _ := sc.sf.Do("", func() (interface{}, error) {
		fips, err := sc.hcc.FloatingIP.AllWithOpts(context.Background(), hcloud.FloatingIPListOpts{
			ListOpts: hcloud.ListOpts{
				LabelSelector: config.FloatingLabelSelector,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("could not fetch floating IPs: %w", err)
		}
		ips := make(fipAllocation, len(fips))
		for _, fip := range fips {
			// TODO: cache server info
			srv, _, err := sc.hcc.Server.GetByID(context.Background(), fip.Server.ID)
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

func getLoadbalancerIPs(svc *corev1.Service) []string {
	ips := make([]string, 0)

	// ignore svc.Spec.LoadBalancerIP; it's provided as a request and may be ignored by k8s

	for _, ip := range svc.Status.LoadBalancer.Ingress {
		if ip.IP != "" {
			ips = append(ips, ip.IP)
		}
	}
	return ips
}
