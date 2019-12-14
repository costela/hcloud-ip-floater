package metallbcontroller

import (
	"fmt"
	"sync"
	"time"

	"github.com/costela/hcloud-ip-floater/internal/config"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const metalLBAddressPoolName = "hcloud-ip-floater"

type Controller struct {
	Logger logrus.FieldLogger
	K8S    *kubernetes.Clientset
	ips    []string
	ipsMu  sync.RWMutex
}

func (mc *Controller) Run() {
	configInformer := informers.NewSharedInformerFactoryWithOptions(
		mc.K8S,
		time.Duration(config.Global.SyncSeconds)*time.Second,
		informers.WithNamespace(config.Global.MetalLBNamespace),
	).Core().V1().ConfigMaps().Informer()

	stopper := make(chan struct{})
	defer close(stopper)

	configInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(newObj interface{}) {
			newCfg, ok := newObj.(*corev1.ConfigMap)
			if !ok {
				mc.Logger.Errorf("received unexpected object type: %T", newObj)
				return
			}
			if err := mc.storeConfig(newCfg); err != nil {
				mc.Logger.WithError(err).Error("error handling new config")
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			newCfg, ok := newObj.(*corev1.ConfigMap)
			if !ok {
				mc.Logger.Errorf("received unexpected new object type: %T", newObj)
				return
			}
			if err := mc.storeConfig(newCfg); err != nil {
				mc.Logger.WithError(err).Error("error handling config update")
			}
		},
		DeleteFunc: func(_ interface{}) {
			if err := mc.storeConfig(nil); err != nil {
				mc.Logger.WithError(err).Error("error removing pod informer")
			}
		},
	})

	configInformer.Run(stopper)
}

func (mc *Controller) storeConfig(cfgM *corev1.ConfigMap) error {
	return nil
}

func (mc *Controller) SetIPs(ips []string) {
	mc.ipsMu.Lock()
	defer mc.ipsMu.Unlock()

	if len(ips) != len(mc.ips) {
	}

	for i := range ips {
		// FIXME: convert to set?
	}
}

// metalLBConfig is a skeleton of MetalLB's config struct (which unfortunately lives in metallb/internal and is
// therefore not importable). It's used to unmarshal the config and setting our pool without interfering with
// unrelated settings.
type metalLBConfig struct {
	Pools          []map[string]interface{} `yaml:"address-pools"`
	Peers          interface{}              `yaml:"peers"`
	BGPCommunities interface{}              `yaml:"bgp-communities"`
}

func mergeConfigs(cfgYaml []byte, ips []string) (*metalLBConfig, error) {
	cfg := &metalLBConfig{}
	if err := yaml.Unmarshal(cfgYaml, cfg); err != nil {
		return nil, fmt.Errorf("could not unmarshal metalLB config: %w", err)
	}

	foundAt := -1
	for i, pool := range cfg.Pools {
		if pool["name"] == metalLBAddressPoolName {
			foundAt = i
		}
	}
	if foundAt >= 0 {
		cfg.Pools[foundAt] = metalLBPoolForIPs(ips)
	} else {
		cfg.Pools = append(cfg.Pools, metalLBPoolForIPs(ips))
	}

	return cfg, nil
}

func metalLBPoolForIPs(ips []string) map[string]interface{} {
	// convert here to make testing a bit easier
	rawIPs := make([]interface{}, len(ips))
	for i := range ips {
		rawIPs[i] = ips[i]
	}

	return map[string]interface{}{
		"name":      metalLBAddressPoolName,
		"protocol":  "layer2",
		"addresses": rawIPs,
	}
}
