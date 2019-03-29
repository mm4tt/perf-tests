/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package probes

import (
	"fmt"
	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/perf-tests/clusterloader2/pkg/errors"
	"k8s.io/perf-tests/clusterloader2/pkg/prometheus"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"k8s.io/klog"
	"k8s.io/perf-tests/clusterloader2/pkg/config"
	"k8s.io/perf-tests/clusterloader2/pkg/framework"
	"k8s.io/perf-tests/clusterloader2/pkg/framework/client"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	checkProbesReadyInterval     = 30 * time.Second
	checkProbesReadyTimeout      = 5 * time.Minute
	currentApiCallMetricsVersion = "v1"
	name                         = "Probes"
	networkLatencyThreshold      = 10 * time.Second // TODO(
	manifestGlob                 = "$GOPATH/src/k8s.io/perf-tests/clusterloader2/pkg/measurements/common/probes/manifests/*.yaml"
	probesNamespace              = "probes"
)

var (
	// TODO(mm4tt): Refactor when new probes/metrics are added.
	networkLatencyMetricThreshold = &measurementutil.LatencyMetric{
		Perc50: networkLatencyThreshold,
		Perc90: networkLatencyThreshold,
		Perc99: networkLatencyThreshold,
	}
)

func init() {
	measurement.Register(name, createProbesMeasurement)
}

func createProbesMeasurement() measurement.Measurement {
	return &probesMeasurement{}
}

type probesMeasurement struct {
	isInitialized    bool
	framework        *framework.Framework
	replicasPerProbe int
	templateMapping  map[string]interface{}
	startTime        time.Time
}

type probesSummary struct {
	probeSummaries []*probeSummary
}

type probeSummary struct {
	name    string
	latency measurementutil.LatencyMetric
}

// SummaryName returns name of the summary.
func (p *probesSummary) SummaryName() string {
	return name
}

// PrintSummary returns summary as a string.
func (p *probesSummary) PrintSummary() (string, error) {
	perfData := &measurementutil.PerfData{Version: currentApiCallMetricsVersion}
	for _, probe := range p.probeSummaries {
		perfData.DataItems = append(perfData.DataItems, measurementutil.DataItem{
			Data: map[string]float64{
				"Perc50": float64(probe.latency.Perc50) / 1000000, // ns -> ms
				"Perc90": float64(probe.latency.Perc90) / 1000000,
				"Perc99": float64(probe.latency.Perc99) / 1000000,
			},
			Unit:   "ms",
			Labels: map[string]string{"Metric": name},
		})
	}
	return util.PrettyPrintJSON(perfData)
}

// Execute supports two actions:
// - reset - Resets latency data on api scheduler side.
// - gather - Gathers and prints current scheduler latency data.
func (p *probesMeasurement) Execute(config *measurement.MeasurementConfig) (summaries []measurement.Summary, err error) {
	if !p.isInitialized {
		p.initialize(config)
	}
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}
	switch action {
	case "start":
		klog.Infof("%s: Starting probes...", p)
		if err := p.start(); err != nil {
			return nil, err
		}
		return summaries, nil
	case "gather":
		summary, err := p.gather()
		if err == nil || errors.IsMetricViolationError(err) {
			summaries = append(summaries, summary)
		}
		return summaries, err
	default:
		return nil, fmt.Errorf("unknown action %v", action)
	}
}

func (p *probesMeasurement) initialize(config *measurement.MeasurementConfig) {
	p.framework = config.ClusterFramework
	p.replicasPerProbe = 1 + config.ClusterFramework.GetClusterConfig().Nodes/200
	p.templateMapping = map[string]interface{}{"Replicas": p.replicasPerProbe}
	p.isInitialized = true
}

func (p *probesMeasurement) start() error {
	k8sClient := p.framework.GetClientSets().GetClient()
	if err := client.CreateNamespace(k8sClient, probesNamespace); err != nil {
		return err
	}
	if err := p.createProbesObjects(); err != nil {
		return err
	}
	if err := p.waitTillProbesReady(); err != nil {
		return err
	}
	p.startTime = time.Now()
	return nil
}

func (p *probesMeasurement) gather() (summary *probesSummary, err error) {
	if p.startTime.IsZero() {
		return summary, fmt.Errorf("measurement %s has not been started", name)
	}

	// 1. Run prometheus query
	// TODO(mm4tt): Implement
	klog.Info("Skipping querying prometheus - not implemented yet...")
	var samples []*model.Sample

	probe := &probeSummary{name: "in_cluster_network_latency"}
	for _, sample := range samples {
		quantile, err := strconv.ParseFloat(string(sample.Metric["quantile"]), 64)
		if err != nil {
			return nil, err
		}
		latency := time.Duration(float64(sample.Value) * float64(time.Second))
		probe.latency.SetQuantile(quantile, latency)
	}
	if err := probe.latency.VerifyThreshold(networkLatencyMetricThreshold); err != nil {
		err = errors.NewMetricViolationError(probe.name, err.Error())
		klog.Errorf("%s: %v", p, err)
	}
	summary.probeSummaries = append(summary.probeSummaries, probe)
	return summary, err
}

func (p *probesMeasurement) createProbesObjects() error {
	templateProvider := config.NewTemplateProvider(filepath.Dir(os.ExpandEnv(manifestGlob)))
	manifests, err := filepath.Glob(manifestGlob)
	if err != nil {
		return err
	}
	for _, manifest := range manifests {
		klog.Infof("Applying %s\n", manifest)
		obj, err := templateProvider.TemplateToObject(filepath.Base(manifest), p.templateMapping)
		if err != nil {
			return err
		}
		if err := p.framework.CreateObject(probesNamespace, obj.GetName(), obj); err != nil {
			return fmt.Errorf("error while applying (%s): %v", manifest, err)
		}
	}
	return nil
}

func (p *probesMeasurement) waitTillProbesReady() error {
	klog.Info("Waiting for Probes to become ready...")
	return wait.Poll(checkProbesReadyInterval, checkProbesReadyTimeout, p.checkProbesReady)
}

func (p *probesMeasurement) checkProbesReady() (bool, error) {
	expectedTargets := p.replicasPerProbe * 2 // ping-server, ping-client
	return prometheus.CheckTargetsReady(
		p.framework.GetClientSets().GetClient(), isProbeTarget, expectedTargets)
}

func isProbeTarget(t prometheus.Target) bool {
	return t.Labels["namespace"] == probesNamespace
}

// Dispose cleans up after the measurement.
func (p *probesMeasurement) Dispose() {
	k8sClient := p.framework.GetClientSets().GetClient()
	if err := client.WaitForDeleteNamespace(k8sClient, probesNamespace); err != nil {
		klog.Fatal(err)
	}
}

// String returns string representation of this measurement.
func (p *probesMeasurement) String() string {
	return name
}
