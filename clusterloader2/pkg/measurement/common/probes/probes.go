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
	"math"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/perf-tests/clusterloader2/pkg/errors"
	"k8s.io/perf-tests/clusterloader2/pkg/flags"
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

	checkProbesReadyInterval = 15 * time.Second
	checkProbesReadyTimeout  = 5 * time.Minute

	currentProbesMetricsVersion = "v1"
)

var (
	probesEnabled     bool
	serviceMonitorGvr = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}
)

func init() {
	flags.BoolEnvVar(&probesEnabled, "enable-probes", "ENABLE_PROBES", false, "Whether to run the probes in the clusterloader2 tests.")
	measurement.Register(name, createProbesMeasurement)
}

func createProbesMeasurement() measurement.Measurement {
	return &probesMeasurement{
		probeNameToPrometheusQueryTmpl: map[string]string{
			"in_cluster_network_latency": "quantile_over_time(0.99, probes:in_cluster_network_latency:histogram_quantile[%v])",
		},
	}
}

type probesMeasurement struct {
	// probeNameToPrometheusQueryTmpl defines a config of this measurement. Updating the config in the
	// createProbesMeasurement method is the only place in go code that needs to be changed while
	// adding a new probe.
	// Each query template should accept a single %v placeholder corresponding to the query window
	// length. See the 'in_cluster_network_latency' as an example.
	probeNameToPrometheusQueryTmpl map[string]string

	framework        *framework.Framework
	replicasPerProbe int
	templateMapping  map[string]interface{}
	startTime        time.Time
}

// Execute supports two actions:
// - start - starts probes and sets up monitoring
// - gather - Gathers and prints metrics.
func (p *probesMeasurement) Execute(config *measurement.MeasurementConfig) ([]measurement.Summary, error) {
	if !probesEnabled {
		klog.Info("Probes are disabled, skipping the measurement execution")
		return nil, nil
	}
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}
	switch action {
	case "start":
		return nil, p.start(config)
	case "gather":
		summary, err := p.gather(config.Params)
		if err != nil && !errors.IsMetricViolationError(err) {
			return nil, err
		}
		return []measurement.Summary{summary}, err
	default:
		return nil, fmt.Errorf("unknown action %v", action)
	}
}

// Dispose cleans up after the measurement.
func (p *probesMeasurement) Dispose() {
	if !probesEnabled {
		klog.Info("Probes are disabled, skipping the Dispose() step")
		return
	}
	klog.Info("Stopping probes...")
	k8sClient := p.framework.GetClientSets().GetClient()
	if err := client.DeleteNamespace(k8sClient, probesNamespace); err != nil {
		klog.Errorf("error while deleting %s namespace: %v", probesNamespace, err)
	}
	if err := client.WaitForDeleteNamespace(k8sClient, probesNamespace); err != nil {
		klog.Errorf("error while waiting for %s namespace to be deleted: %v", probesNamespace, err)
	}
}

// String returns string representation of this measurement.
func (p *probesMeasurement) String() string {
	return name
}

func (p *probesMeasurement) initialize(config *measurement.MeasurementConfig) error {
	replicasPerProbe, err := util.GetInt(config.Params, "replicasPerProbe")
	if err != nil {
		return err
	}
	p.framework = config.ClusterFramework
	p.replicasPerProbe = replicasPerProbe
	p.templateMapping = map[string]interface{}{"Replicas": replicasPerProbe}
	klog.Info(p.templateMapping)
	return nil
}

func (p *probesMeasurement) start(config *measurement.MeasurementConfig) error {
	klog.Info("Starting probes...")
	if !p.startTime.IsZero() {
		return fmt.Errorf("measurement %s cannot be started twice", name)
	}
	if err := p.initialize(config); err != nil {
		return err
	}
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
	klog.Info("Gathering metrics from probes...")
	if p.startTime.IsZero() {
		return nil, fmt.Errorf("measurement %s has not been started", name)
	}
	thresholds, err := parseThresholds(params)
	if err != nil {
		return nil, err
	}
	measurementEnd := time.Now()
	var probeSummaries []*probeSummary
	for probeName, queryTmpl := range p.probeNameToPrometheusQueryTmpl {
		query := prepareQuery(queryTmpl, p.startTime, measurementEnd)
		samples, err := executePrometheusQuery(p.framework.GetClientSets().GetClient(), query, measurementEnd)
		if err != nil {
			return nil, err
		}
		probe := &probeSummary{name: probeName}
		for _, sample := range samples {
			quantile, err := strconv.ParseFloat(string(sample.Metric["quantile"]), 64)
			if err != nil {
				return nil, err
			}
			latency := time.Duration(float64(sample.Value) * float64(time.Second))
			probe.latency.SetQuantile(quantile, latency)
		}
		if threshold, ok := thresholds[probeName]; ok {
			if err := probe.latency.VerifyThreshold(threshold); err != nil {
				err = errors.NewMetricViolationError(probe.name, err.Error())
				klog.Errorf("%s: %v", p, err)
			}
		}
		probeSummaries = append(probeSummaries, probe)
	}
	return &probesSummary{probeSummaries: probeSummaries}, err
}

func (p *probesMeasurement) createProbesObjects() error {
	return p.framework.ApplyTemplatedManifests(manifestGlob, p.templateMapping)
}

func (p *probesMeasurement) waitTillProbesReady() error {
	klog.Info("Waiting for Probes to become ready...")
	return wait.Poll(checkProbesReadyInterval, checkProbesReadyTimeout, p.checkProbesReady)
}

func (p *probesMeasurement) checkProbesReady() (bool, error) {
	serviceMonitors, err := p.framework.GetDynamicClients().GetClient().
		Resource(serviceMonitorGvr).Namespace(probesNamespace).List(metav1.ListOptions{})
	if err != nil {
		if client.IsRetryableAPIError(err) || client.IsRetryableNetError(err) {
			err = nil // Retryable error, don't return it.
		}
		return false, err
	}
	expectedTargets := p.replicasPerProbe * len(serviceMonitors.Items)
	return prometheus.CheckTargetsReady(
		p.framework.GetClientSets().GetClient(), isProbeTarget, expectedTargets)
}

func isProbeTarget(t prometheus.Target) bool {
	return t.Labels["namespace"] == probesNamespace
}

func parseThresholds(params map[string]interface{}) (map[string]*measurementutil.LatencyMetric, error) {
	thresholds := make(map[string]*measurementutil.LatencyMetric)
	for name, thresholdVal := range params["thresholds"].(map[string]interface{}) {
		threshold, err := time.ParseDuration(thresholdVal.(string))
		if err != nil {
			return nil, err
		}
		thresholds[name] = makeLatencyThreshold(threshold)
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

func prepareQuery(queryTemplate string, startTime, endTime time.Time) string {
	measurementDuration := endTime.Sub(startTime)
	return fmt.Sprintf(queryTemplate, measurementutil.ToPrometheusTime(measurementDuration))
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

// TODO(mm4tt): Remove the method below and start using the one from common util once it's available.
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
