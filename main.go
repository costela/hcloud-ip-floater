package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/sirupsen/logrus"
	"github.com/stevenroose/gonfig"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	serviceName = "hcloud-ip-floater"
	version     = "unreleased"
)

var config struct {
	LogLevel              string `id:"log-level" short:"l" desc:"verbosity level for logs" default:"warn"`
	HCloudToken           string `id:"hcloud-token" desc:"API token for HCloud access"`
	ServiceLabelSelector  string `id:"service-label-selector" desc:"label selector used to match services" default:"hcloud-ip-floater.cstl.dev/ignore!=true"`
	FloatingLabelSelector string `id:"floating-label-selector" desc:"label selector used to match floating IPs" default:""`

	// optional MetalLB integration
	MetalLBNamespace  string `id:"metallb-namespace" desc:"namespace to create MetalLB ConfigMap"`
	MetalLBConfigName string `id:"metallb-config-name" desc:"name of ConfigMap resource used by MetalLB"`
}

func main() {
	logger := logrus.New()

	if err := gonfig.Load(&config, gonfig.Conf{
		EnvPrefix:         "HCLOUD_IP_FLOATER_",
		FlagIgnoreUnknown: false,
	}); err != nil {
		logger.Fatalf("could not parse options: %s", err)
	}

	if level, err := logrus.ParseLevel(config.LogLevel); err != nil {
		logger.Fatalf("could not set log level to %s: %s", config.LogLevel, err)
	} else {
		logger.SetLevel(level)
	}

	var k8sCfg *rest.Config
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			logger.Fatalf("could not init in-cluster config: %s", err)
		}
		k8sCfg = cfg
	} else {
		cfg, err := clientcmd.BuildConfigFromKubeconfigGetter("", clientcmd.NewDefaultClientConfigLoadingRules().Load)
		if err != nil {
			logger.Fatalf("could not init in-cluster config: %w", err)
		}
		k8sCfg = cfg
	}

	k8s, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		logger.Fatalf("could not init k8s client: %w", err)
	}

	hcc := hcloud.NewClient(
		hcloud.WithApplication(serviceName, version),
		hcloud.WithToken(config.HCloudToken),
		hcloud.WithBackoffFunc(hcloud.ExponentialBackoff(2.5, time.Second)), // TODO: this has no upper bound
		hcloud.WithDebugWriter(logger.WithFields(logrus.Fields{"component": "hcloud"}).WriterLevel(logrus.DebugLevel)),
	)

	sc := serviceController{
		logger: logger,
		k8s:    k8s,
		hcc:    hcc,
	}

	sc.run()
}

func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	user, err := user.Current()
	if err != nil {
		return path
	}
	return filepath.Join(user.HomeDir, strings.TrimPrefix(path, "~/"))
}
