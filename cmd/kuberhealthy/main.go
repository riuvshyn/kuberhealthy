// Copyright 2018 Comcast Cable Communications Management, LLC
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Kuberhealthy is an enhanced health check for Kubernetes clusters.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/integrii/flaggy"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Comcast/kuberhealthy/pkg/khcheckcrd"
	"github.com/Comcast/kuberhealthy/pkg/masterCalculation"
)

// status represents the current Kuberhealthy OK:Error state
var kubeConfigFile = filepath.Join(os.Getenv("HOME"), ".kube", "config")
var listenAddress = ":8080"
var podCheckNamespaces = "kube-system"
var dnsEndpoints []string
var podNamespace = os.Getenv("POD_NAMESPACE")
var isMaster bool // indicates this instance is the master and should be running checks

// shutdown signal handling
var sigChan chan os.Signal
var doneChan chan bool
var terminationGracePeriod = time.Minute * 5 // keep calibrated with kubernetes terminationGracePeriodSeconds

// flags indicating that checks of specific types should be used
var enableForceMaster bool // force master mode - for debugging
var enableDebug bool       // enable debug logging
// DSPauseContainerImageOverride specifies the sleep image we will use on the daemonset checker
var DSPauseContainerImageOverride string // specify an alternate location for the DSC pause container - see #114
var logLevel = "info"
var enableComponentStatusChecks = true
var enableDaemonSetChecks = true
var enablePodRestartChecks = true
var enablePodStatusChecks = true
var enableDnsStatusChecks = true
var enableExternalChecks = true

// InfluxDB connection configuration
var enableInflux = false
var influxUrl = ""
var influxUsername = ""
var influxPassword = ""
var influxDB = "http://localhost:8086"
var kuberhealthy *Kuberhealthy

// constants for using the kuberhealthy status CRD
const statusCRDGroup = "comcast.github.io"
const statusCRDVersion = "v1"
const statusCRDResource = "khstates"

// constants for using the kuberhealthy check CRD
const checkCRDGroup = "comcast.github.io"
const checkCRDVersion = "v1"
const checkCRDResource = "khchecks"
var checkCRDScanInterval = time.Second * 15 // how often we scan for changes to check CRD objects

func init() {
	flaggy.SetDescription("Kuberhealthy is an in-cluster synthetic health checker for Kubernetes.")
	flaggy.String(&kubeConfigFile, "", "kubecfg", "(optional) absolute path to the kubeconfig file")
	flaggy.String(&listenAddress, "l", "listenAddress", "The port for kuberhealthy to listen on for web requests")
	flaggy.Bool(&enableComponentStatusChecks, "", "componentStatusChecks", "Set to false to disable daemonset deployment checking.")
	flaggy.Bool(&enableDaemonSetChecks, "", "daemonsetChecks", "Set to false to disable cluster daemonset deployment and termination checking.")
	flaggy.Bool(&enablePodRestartChecks, "", "podRestartChecks", "Set to false to disable pod restart checking.")
	flaggy.Bool(&enablePodStatusChecks, "", "podStatusChecks", "Set to false to disable pod lifecycle phase checking.")
	flaggy.Bool(&enableDnsStatusChecks, "", "dnsStatusChecks", "Set to false to disable DNS checks.")
	flaggy.Bool(&enableExternalChecks, "", "externalChecks", "Set to false to disable external checks.")
	flaggy.Bool(&enableForceMaster, "", "forceMaster", "Set to true to enable local testing, forced master mode.")
	flaggy.Bool(&enableDebug, "d", "debug", "Set to true to enable debug.")
	flaggy.String(&DSPauseContainerImageOverride, "", "dsPauseContainerImageOverride", "Set an alternate image location for the pause container the daemon set checker uses for its daemon set configuration.")
	flaggy.String(&podCheckNamespaces, "", "podCheckNamespaces", "The comma separated list of namespaces on which to check for pod status and restarts, if enabled.")
	flaggy.String(&logLevel, "", "log-level", fmt.Sprintf("Log level to be used one of [%s].", getAllLogLevel()))
	flaggy.StringSlice(&dnsEndpoints, "", "dnsEndpoints", "The comma separated list of dns endpoints to check, if enabled. Defaults to kubernetes.default")
	// Influx flags
	flaggy.String(&influxUsername, "", "influxUser", "Username for the InfluxDB instance")
	flaggy.String(&influxPassword, "", "influxPassword", "Password for the InfluxDB instance")
	flaggy.String(&influxUrl, "", "influxUrl", "Address for the InfluxDB instance")
	flaggy.String(&influxDB, "", "influxDB", "Name of the InfluxDB database")
	flaggy.Bool(&enableInflux, "", "enableInflux", "Set to true to enable metric forwarding to Influx DB.")
	flaggy.Parse()

	parsedLogLevel, err := log.ParseLevel(logLevel)
	if err != nil {
		log.Fatalln("Unable to parse log-level flag: ", err)
	}

	// log to stdout and set the level to info by default
	log.SetOutput(os.Stdout)
	log.SetLevel(parsedLogLevel)
	log.Infoln("Startup Arguments:", os.Args)

	// handle debug logging
	if enableDebug {
		log.SetLevel(log.DebugLevel)
		masterCalculation.EnableDebug()
		log.Infoln("Enabling debug logging")
	}

	// shutdown signal handling
	// we give a queue depth here to prevent blocking in some cases
	sigChan = make(chan os.Signal, 5)
	doneChan = make(chan bool, 5)

	// Handle force master mode
	if enableForceMaster {
		log.Infoln("Enabling forced master mode")
		masterCalculation.DebugAlwaysMasterOn()
	}
}

func main() {

	// start listening for shutdown interrupts
	go listenForInterrupts()

	// Create a new Kuberhealthy struct
	kuberhealthy = NewKuberhealthy()
	kuberhealthy.ListenAddr = listenAddress

	// tell Kuberhealthy to start all checks and master change monitoring
	go kuberhealthy.Start()

	// Start the web server and restart it if it crashes
	kuberhealthy.StartWebServer()
}

// listenForInterrupts watches for termination signals and acts on them
func listenForInterrupts() {
	signal.Notify(sigChan, os.Interrupt, os.Kill)
	<-sigChan
	log.Infoln("Shutting down...")
	go kuberhealthy.Shutdown()
	// wait for checks to be done shutting down before exiting
	select {
	case <-doneChan:
		log.Infoln("Shutdown gracefully completed!")
	case <-sigChan:
		log.Warningln("Shutdown forced from multiple interrupts!")
	case <-time.After(terminationGracePeriod):
		log.Errorln("Shutdown took too long.  Shutting down forcefully!")
	}
	os.Exit(0)
}

// getWhitelistedUUIDForExternalCheck fetches the current allowed UUID for an
// external check.  This data is stored in khcheck custom resources.
func getWhitelistedUUIDForExternalCheck(checkName string) (string, error) {
	// make a new crd check client
	checkClient, err := khcheckcrd.Client(checkCRDGroup, checkCRDVersion, kubeConfigFile)
	if err != nil {
		return "", err
	}

	r, err := checkClient.Get(metav1.GetOptions{}, checkCRDResource, checkName)
	if err != nil {
		return "", err
	}

	return r.Spec.CurrentUUID, nil
}
