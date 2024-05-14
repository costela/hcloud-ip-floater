package manualcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sort"
	"strings"
	"time"

	"github.com/costela/hcloud-ip-floater/internal/config"
	"github.com/costela/hcloud-ip-floater/internal/fipcontroller"
	"github.com/costela/hcloud-ip-floater/internal/servicecontroller"
	"github.com/costela/hcloud-ip-floater/internal/stringset"
	"github.com/costela/hcloud-ip-floater/internal/utils"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type Controller struct {
	Logger logrus.FieldLogger

	K8S  *kubernetes.Clientset
	SVCc *servicecontroller.Controller
	FIPc *fipcontroller.Controller

	podInformer cache.SharedInformer
}

func (c *Controller) Run() {
	ipLabel := config.Global.ManualAssignmentLabel

	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		c.K8S,
		time.Duration(config.Global.SyncSeconds)*time.Second,
		informers.WithTweakListOptions(func(listOpts *metav1.ListOptions) {
			listOpts.LabelSelector = ipLabel
		}),
	)
	podInformer := podInformerFactory.Core().V1().Pods().Informer()
	c.podInformer = podInformer

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				c.Logger.Errorf("received unexpected object type: %T", obj)
				return
			}

			c.Logger.WithFields(logrus.Fields{
				"namespace": pod.Namespace,
				"name":      pod.Name,
				"node":      pod.Spec.NodeName,
			}).Info("New pod")

			value := pod.Labels[ipLabel]
			ips := parseIPList(value)
			if len(ips) == 0 {
				c.Logger.Debug("label not present or empty")
				return
			}

			for ip := range ips {
				c.reconcileIP(ip)
			}
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			oldPod, ok := oldObj.(*corev1.Pod)
			if !ok {
				c.Logger.Errorf("received unexpected object type: %T", oldObj)
				return
			}
			newPod, ok := newObj.(*corev1.Pod)
			if !ok {
				c.Logger.Errorf("received unexpected object type: %T", newObj)
				return
			}

			c.Logger.WithFields(logrus.Fields{
				"namespace": newPod.Namespace,
				"name":      newPod.Name,
				"node":      newPod.Spec.NodeName,
			}).Info("Pod updated")

			// diff label values
			oldValue := oldPod.Labels[ipLabel]
			oldIPs := parseIPList(oldValue)
			newValue := newPod.Labels[ipLabel]
			newIPs := parseIPList(newValue)

			removedIPs := oldIPs.Diff(newIPs)
			for ip := range removedIPs {
				c.reconcileIP(ip)
			}

			for ip := range newIPs {
				c.reconcileIP(ip)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				c.Logger.Errorf("received unexpected object type: %T", obj)
				return
			}

			c.Logger.WithFields(logrus.Fields{
				"namespace": pod.Namespace,
				"name":      pod.Name,
				"node":      pod.Spec.NodeName,
			}).Info("Pod deleted")

			value := pod.Labels[ipLabel]
			ips := parseIPList(value)
			if len(ips) == 0 {
				c.Logger.Debug("label not present or empty")
				return
			}

			for ip := range ips {
				c.reconcileIP(ip)
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	podInformer.Run(ctx.Done())
}

func parseIPList(value string) stringset.StringSet {
	set := make(stringset.StringSet)
	ips := strings.Split(value, ",")
	for _, ip := range ips {
		set.Add(strings.TrimSpace(ip))
	}
	return set
}

func singleSet(value string) stringset.StringSet {
	set := make(stringset.StringSet)
	set.Add(value)
	return set
}

// only use with a locked knownIPmu!
func (c *Controller) reconcileIP(ip string) {
	log := c.Logger.WithField("ip", ip)

	if c.SVCc.HasServiceIP(ip) {
		log.Warn("IP is assigned to a service, cannot use manually on a pod")
		return
	}

	pods := c.podInformer.GetStore().List()

	nodes := make([]string, 0, len(pods))
	for _, pod := range pods {
		pod := pod.(*corev1.Pod)
		if !utils.PodIsReady(pod) {
			continue
		}
		ips := parseIPList(pod.Labels[config.Global.ManualAssignmentLabel])
		for labelIP := range ips {
			if labelIP == ip {
				nodes = append(nodes, pod.Spec.NodeName)
				break
			}
		}
	}

	ipSet := singleSet(ip)

	if len(nodes) == 0 {
		log.Info("None of the pods are ready")
		c.FIPc.ForgetAttachments(ipSet)
		return
	}

	sort.Slice(nodes, func(i, j int) bool {
		a := sha256.Sum256([]byte(nodes[i]))
		b := sha256.Sum256([]byte(nodes[j]))
		return bytes.Compare(a[:], b[:]) > 0
	})

	electedNode := nodes[0]
	c.FIPc.AttachToNode(ipSet, electedNode)
	log.WithField("node", electedNode).Info("Attached IP using manual assignment")
}
