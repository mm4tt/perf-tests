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
	"k8s.io/client-go/kubernetes"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"k8s.io/perf-tests/clusterloader2/pkg/config"
	"k8s.io/perf-tests/clusterloader2/pkg/errors"
	"k8s.io/perf-tests/clusterloader2/pkg/framework"
	"k8s.io/perf-tests/clusterloader2/pkg/framework/client"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/prometheus"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	name            = "Probes"
	probesNamespace = "probes"

	manifestGlob = "$GOPATH/src/k8s.io/perf-tests/clusterloader2/pkg/measurement/common/probes/manifests/*.yaml"

	checkProbesReadyInterval = 30 * time.Second
	checkProbesReadyTimeout  = 5 * time.Minute

	currentProbesMetricsVersion = "v1"
	inClusterNetworkLatencyName = "in_cluster_network_latency"
	// inClusterNetworkLatencyQuery - %v should be replaced with query window size.
	inClusterNetworkLatencyQuery = "quantile_over_time(0.99, probes:in_cluster_network_latency:histogram_quantile[%v])"
	queryWindowSize              = 5 * time.Minute
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
	perfData := &measurementutil.PerfData{Version: currentProbesMetricsVersion}
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
// - start - starts probes and sets up monitoring
// - gather - Gathers and prints metrics.
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
		klog.Infof("%s: Gathering metrics from probes...", p)
		summary, err := p.gather(config.Params)
		if err == nil || errors.IsMetricViolationError(err) {
			summaries = append(summaries, summary)
		}
		return summaries, err
	default:
		return nil, fmt.Errorf("unknown action %v", action)
	}
}

// Dispose cleans up after the measurement.
func (p *probesMeasurement) Dispose() {
	klog.Infof("%s: Stopping probes...", p)
	k8sClient := p.framework.GetClientSets().GetClient()
	if err := client.WaitForDeleteNamespace(k8sClient, probesNamespace); err != nil {
		klog.Fatal(err)
	}
}

// String returns string representation of this measurement.
func (p *probesMeasurement) String() string {
	return name
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

func (p *probesMeasurement) gather(params map[string]interface{}) (*probesSummary, error) {
	if p.startTime.IsZero() {
		return nil, fmt.Errorf("measurement %s has not been started", name)
	}
	thresholds, err := parseAndValidateThresholds(params)
	if err != nil {
		return nil, err
	}
	measurementEnd := time.Now()
	// TODO(mm4tt): Rewrite when adding new probe to make it more generic.
	query := prepareQuery(inClusterNetworkLatencyQuery, p.startTime, measurementEnd)
	samples, err := executePrometheusQuery(p.framework.GetClientSets().GetClient(), query, measurementEnd)
	if err != nil {
		return nil, err
	}
	probe := &probeSummary{name: inClusterNetworkLatencyName}
	for _, sample := range samples {
		quantile, err := strconv.ParseFloat(string(sample.Metric["quantile"]), 64)
		if err != nil {
			return nil, err
		}
		latency := time.Duration(float64(sample.Value) * float64(time.Second))
		probe.latency.SetQuantile(quantile, latency)
	}
	if err := probe.latency.VerifyThreshold(thresholds[inClusterNetworkLatencyName]); err != nil {
		err = errors.NewMetricViolationError(probe.name, err.Error())
		klog.Errorf("%s: %v", p, err)
	}
	return &probesSummary{probeSummaries: []*probeSummary{probe}}, err
}

func (p *probesMeasurement) createProbesObjects() error {
	expandedManifestGlob := os.ExpandEnv(manifestGlob)
	templateProvider := config.NewTemplateProvider(filepath.Dir(expandedManifestGlob))
	manifests, err := filepath.Glob(expandedManifestGlob)
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
	expectedTargets := p.replicasPerProbe * 2 // ping-client, ping-server
	return prometheus.CheckTargetsReady(
		p.framework.GetClientSets().GetClient(), isProbeTarget, expectedTargets)
}

func isProbeTarget(t prometheus.Target) bool {
	return t.Labels["namespace"] == probesNamespace
}

func parseAndValidateThresholds(params map[string]interface{}) (map[string]*measurementutil.LatencyMetric, error) {
	thresholds := make(map[string]*measurementutil.LatencyMetric)
	for name, thresholdVal := range params["thresholds"].(map[string]interface{}) {
		threshold, err := time.ParseDuration(thresholdVal.(string))
		if err != nil {
			return nil, err
		}
		thresholds[name] = makeLatencyThreshold(threshold)
	}
	if _, ok := thresholds[inClusterNetworkLatencyName]; !ok {
		return nil, fmt.Errorf("missing threshold for %s", name)
	}
	return thresholds, nil
}

func makeLatencyThreshold(threshold time.Duration) *measurementutil.LatencyMetric {
	return &measurementutil.LatencyMetric{
		Perc50: threshold,
		Perc90: threshold,
		Perc99: threshold,
	}
}

// TODO(mm4tt): Remove the methods below and start using the ones from common util once it's created.

func prepareQuery(queryTemplate string, startTime, endTime time.Time) string {
	measurementDuration := endTime.Sub(startTime)
	return fmt.Sprintf(queryTemplate, measurementutil.ToPrometheusTime(measurementDuration))
}

func executePrometheusQuery(c kubernetes.Interface, query string, queryTime time.Time) ([]*model.Sample, error) {
	if queryTime.IsZero() {
		return nil, fmt.Errorf("query time can't be zero")
	}
	var body []byte
	var queryErr error
	params := map[string]string{
		"query": query,
		"time":  queryTime.Format(time.RFC3339),
	}
	if err := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		body, queryErr = c.CoreV1().
			Services("monitoring").
			ProxyGet("http", "prometheus-k8s", "9090", "api/v1/query", params).
			DoRaw()
		if queryErr != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		if queryErr != nil {
			return nil, fmt.Errorf("query error: %v", queryErr)
		}
		return nil, fmt.Errorf("query error: %v", err)
	}
	samples, err := measurementutil.ExtractMetricSamples2(body)
	if err != nil {
		return nil, fmt.Errorf("exctracting error: %v", err)
	}
	var resultSamples []*model.Sample
	for _, sample := range samples {
		if !math.IsNaN(float64(sample.Value)) {
			resultSamples = append(resultSamples, sample)
		}
	}
	return resultSamples, nil
}
