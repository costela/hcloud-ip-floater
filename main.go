package main

import (
	"fmt"
	"os"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/sirupsen/logrus"
	"github.com/stevenroose/gonfig"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/costela/hcloud-ip-floater/internal/config"
	"github.com/costela/hcloud-ip-floater/internal/fipcontroller"
	"github.com/costela/hcloud-ip-floater/internal/servicecontroller"
)

const (
	serviceName = "hcloud-ip-floater"
)

var version = "unreleased"

func main() {
	logger := logrus.New()

	if err := gonfig.Load(&config.Global, gonfig.Conf{
		EnvPrefix:         "HCLOUD_IP_FLOATER_",
		FlagIgnoreUnknown: false,
	}); err != nil {
		logger.Fatalf("could not parse options: %s", err)
	}

	if config.Global.Version {
		fmt.Println(version)
		os.Exit(0)
	}

	if level, err := logrus.ParseLevel(config.Global.LogLevel); err != nil {
		logger.Fatalf("could not set log level to %s: %s", config.Global.LogLevel, err)
	} else {
		logger.SetLevel(level)
	}

	logger.WithFields(logrus.Fields{"version": version}).Info("starting hcloud IP floater")

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
			logger.Fatalf("could not init in-cluster config: %s", err)
		}
		k8sCfg = cfg
	}

	k8s, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		logger.Fatalf("could not init k8s client: %s", err)
	}

	hcc := hcloud.NewClient(
		hcloud.WithApplication(serviceName, version),
		hcloud.WithToken(config.Global.HCloudToken),
		hcloud.WithDebugWriter(logger.WithFields(logrus.Fields{"component": "hcloud"}).WriterLevel(logrus.DebugLevel)),
	)

	fipc := fipcontroller.New(logger, hcc)

	sc := servicecontroller.Controller{
		Logger: logger,
		K8S:    k8s,
		FIPc:   fipc,
	}

	go fipc.Run()
	go sc.Run()

	select {}
}
