package main

import (
	"flag"
	"net/http"
	"strconv"
	"strings"

	"github.com/sstarcher/helm-exporter/config"
	"github.com/sstarcher/helm-exporter/registries"

	cmap "github.com/orcaman/concurrent-map"

	log "github.com/sirupsen/logrus"

	"os"

	// Import to initialize client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/facebookgo/flagenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	cfg      config.Config
	settings = cli.New()
	clients  = cmap.New()

	namespaces = flag.String("namespaces", "", "namespaces to monitor.  Defaults to all")
	configFile = flag.String("config", "", "Configfile to load for helm overwrite registries.  Default is empty")

	infoMetric             = flag.Bool("info-metric", true, "Generate info metric.  Defaults to true")
	timestampMetric        = flag.Bool("timestamp-metric", true, "Generate timestamps metric.  Defaults to true")
	countAllReleasesMetric = flag.Bool("count-all-releases-metric", true, "Generate count metric for all Helm releases. Defaults to true")

	fetchLatest = flag.Bool("latest-chart-version", true, "Attempt to fetch the latest chart version from registries. Defaults to true")

	statusCodeMap = map[string]float64{
		"unknown":          0,
		"deployed":         1,
		"uninstalled":      2,
		"superseded":       3,
		"failed":           -1,
		"uninstalling":     5,
		"pending-install":  6,
		"pending-upgrade":  7,
		"pending-rollback": 8,
	}
)

// helmCollector is the struct for the Helm collector that contains
// pointers to prometheus descriptors for each metric.
type helmCollector struct {
	revisionsCounterDesc    *prometheus.Desc
	statsInfoGaugeDesc      *prometheus.Desc
	statsTimestampGaugeDesc *prometheus.Desc
}

func initFlags() config.AppConfig {
	cliFlags := new(config.AppConfig)
	cliFlags.ConfigFile = *configFile
	return *cliFlags
}

// newHelmCollector creates a constructor for the Helm collector that
// initializes every descriptor and returns a pointer to the collector.
func newHelmCollector() *helmCollector {
	return &helmCollector{
		revisionsCounterDesc: prometheus.NewDesc(
			"helm_chart_all_releases_total",
			"Total Number of all Helm releases",
			[]string{"chart", "release", "namespace"},
			nil,
		),
		statsInfoGaugeDesc: prometheus.NewDesc(
			"helm_chart_info",
			"Information on helm releases",
			[]string{"chart", "release", "version", "appVersion", "updated", "namespace", "latestVersion"},
			nil,
		),
		statsTimestampGaugeDesc: prometheus.NewDesc(
			"helm_chart_timestamp",
			"Timestamps of helm releases",
			[]string{"chart", "release", "version", "appVersion", "updated", "namespace", "latestVersion"},
			nil,
		),
	}
}

func getLatestChartVersionFromHelm(name string, helmRegistries registries.HelmRegistries) (version string) {
	version = helmRegistries.GetLatestVersionFromHelm(name)
	log.WithField("chart", name).Debugf("last chart repo version is  %v", version)
	return
}

func healthz(w http.ResponseWriter, r *http.Request) {

}

func connect(namespace string) {
	actionConfig := new(action.Configuration)
	err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), log.Infof)
	if err != nil {
		log.Warnf("failed to connect to %s with %v", namespace, err)
	} else {
		log.Infof("Watching namespace %s", namespace)
		clients.Set(namespace, actionConfig)
	}
}

func informer() {
	actionConfig := new(action.Configuration)
	err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), log.Infof)
	if err != nil {
		log.Fatal(err)
	}

	clientset, err := actionConfig.KubernetesClientSet()
	if err != nil {
		log.Fatal(err)
	}

	factory := informers.NewSharedInformerFactory(clientset, 0)
	informer := factory.Core().V1().Namespaces().Informer()
	stopper := make(chan struct{})
	defer close(stopper)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// "k8s.io/apimachinery/pkg/apis/meta/v1" provides an Object
			// interface that allows us to get metadata easily
			mObj := obj.(v1.Object)
			connect(mObj.GetName())
		},
		DeleteFunc: func(obj interface{}) {
			mObj := obj.(v1.Object)
			log.Infof("Removing namespace %s", mObj.GetName())
			clients.Remove(mObj.GetName())
		},
	})

	informer.Run(stopper)
}

// Describe implements prometheus.Collector.
// Each and every collector must implement the Describe function.
// It essentially writes all descriptors to the prometheus desc channel.
func (c *helmCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.revisionsCounterDesc
	if *infoMetric == true {
		ch <- c.statsInfoGaugeDesc
	}
	if *timestampMetric == true {
		ch <- c.statsTimestampGaugeDesc
	}
}

// Collect implements prometheus.Collector.
// Collect implements required collect function for all promehteus collectors
func (c *helmCollector) Collect(ch chan<- prometheus.Metric) {
	for _, client := range clients.Items() {
		list := action.NewList(client.(*action.Configuration))
		items, err := list.Run()
		if err != nil {
			log.Warnf("got error while listing %v", err)
			continue
		}

		for _, item := range items {
			chart := item.Chart.Name()
			releaseName := item.Name
			version := item.Chart.Metadata.Version
			appVersion := item.Chart.AppVersion()
			updated := item.Info.LastDeployed.Unix() * 1000
			namespace := item.Namespace
			status := statusCodeMap[item.Info.Status.String()]
			latestVersion := ""

			if *fetchLatest {
				latestVersion = getLatestChartVersionFromHelm(item.Chart.Name(), cfg.HelmRegistries)
			}

			helmRevisionsLabelValues := []string{chart, releaseName, namespace}
			helmStatsInfoLabelValues := []string{chart, releaseName, version, appVersion, strconv.FormatInt(updated, 10), namespace, latestVersion}
			helmStatsTimestampLabelValues := []string{chart, releaseName, version, appVersion, strconv.FormatInt(updated, 10), namespace, latestVersion}
			ch <- prometheus.MustNewConstMetric(
				c.revisionsCounterDesc,
				prometheus.CounterValue,
				float64(item.Version),
				helmRevisionsLabelValues...,
			)

			if *infoMetric == true {
				ch <- prometheus.MustNewConstMetric(
					c.statsInfoGaugeDesc,
					prometheus.GaugeValue,
					status,
					helmStatsInfoLabelValues...,
				)
			}
			if *timestampMetric == true {
				ch <- prometheus.MustNewConstMetric(
					c.statsTimestampGaugeDesc,
					prometheus.GaugeValue,
					float64(updated),
					helmStatsTimestampLabelValues...,
				)
			}

		}
	}
}

func init() {
	// Metrics have to be registered to be exposed.
	prometheus.MustRegister(newHelmCollector())
}

func main() {
	flagenv.Parse()
	flag.Parse()
	cliFlags := initFlags()
	cfg = config.LoadConfiguration(cliFlags.ConfigFile)

	if namespaces == nil || *namespaces == "" {
		go informer()
	} else {
		for _, namespace := range strings.Split(*namespaces, ",") {
			connect(namespace)
		}
	}

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", healthz)
	log.Fatal(http.ListenAndServe(":9571", nil))
}
