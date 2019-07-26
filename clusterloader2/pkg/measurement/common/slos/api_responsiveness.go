/*
Copyright 2018 The Kubernetes Authors.

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

package slos

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/perf-tests/clusterloader2/pkg/errors"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	bigClusterNodeCountThreshold = 500
	// We are setting 1s threshold for apicalls even in small clusters to avoid flakes.
	// The problem is that if long GC is happening in small clusters (where we have e.g.
	// 1-core master machines) and tests are pretty short, it may consume significant
	// portion of CPU and basically stop all the real work.
	// Increasing threshold to 1s is within our SLO and should solve this problem.
	apiCallLatencyThreshold time.Duration = 180 * time.Millisecond

	// We use a higher threshold for list apicalls if the cluster is big (i.e having > 500 nodes)
	// as list response sizes are bigger in general for big clusters. We also use a higher threshold
	// for list calls at cluster scope (this includes non-namespaced and all-namespaced calls).
	apiListCallLatencyThreshold      time.Duration = 5 * time.Second
	apiClusterScopeListCallThreshold time.Duration = 10 * time.Second

	currentAPICallMetricsVersion = "v1"

	apiResponsivenessMeasurementName = "APIResponsiveness"
)

func init() {
	if err := measurement.Register(apiResponsivenessMeasurementName, createAPIResponsivenessMeasurement); err != nil {
		klog.Fatalf("Cannot register %s: %v", apiResponsivenessMeasurementName, err)
	}
}

func createAPIResponsivenessMeasurement() measurement.Measurement {
	return &apiResponsivenessMeasurement{}
}

type apiResponsivenessMeasurement struct{}

// Execute supports two actions:
// - reset - Resets latency data on api server side.
// - gather - Gathers and prints current api server latency data.
func (a *apiResponsivenessMeasurement) Execute(config *measurement.MeasurementConfig) ([]measurement.Summary, error) {
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}

	switch action {
	case "reset":
		klog.Infof("%s: resetting latency metrics in apiserver...", a)
		return nil, apiserverMetricsReset(config.ClusterFramework.GetClientSets().GetClient())
	case "gather":
		// TODO(krzysied): Implement new method of collecting latency metrics.
		// New method is defined here: https://github.com/kubernetes/community/blob/master/sig-scalability/slos/slos.md#steady-state-slisslos.
		nodeCount, err := util.GetIntOrDefault(config.Params, "nodeCount", config.ClusterFramework.GetClusterConfig().Nodes)
		if err != nil {
			return nil, err
		}
		summary, err := a.apiserverMetricsGather(config.ClusterFramework.GetClientSets().GetClient(), nodeCount)
		if err != nil && !errors.IsMetricViolationError(err) {
			return nil, err
		}
		return []measurement.Summary{summary}, err
	default:
		return nil, fmt.Errorf("unknown action %v", action)
	}
}

// Dispose cleans up after the measurement.
func (*apiResponsivenessMeasurement) Dispose() {}

// String returns string representation of this measurement.
func (*apiResponsivenessMeasurement) String() string {
	return apiResponsivenessMeasurementName
}

func (a *apiResponsivenessMeasurement) apiserverMetricsGather(c clientset.Interface, nodeCount int) (measurement.Summary, error) {
	isBigCluster := (nodeCount > bigClusterNodeCountThreshold)
	metrics, err := readLatencyMetrics(c)
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(metrics))
	var badMetrics []string
	top := 5
	for i := range metrics.ApiCalls {
		latency := metrics.ApiCalls[i].Latency.Perc99
		isListCall := (metrics.ApiCalls[i].Verb == "LIST")
		isClusterScopedCall := (metrics.ApiCalls[i].Scope == "cluster")
		isBad := false
		latencyThreshold := apiCallLatencyThreshold
		if isListCall && isBigCluster {
			latencyThreshold = apiListCallLatencyThreshold
			if isClusterScopedCall {
				latencyThreshold = apiClusterScopeListCallThreshold
			}
		}
		if latency > latencyThreshold {
			isBad = true
			badMetrics = append(badMetrics, fmt.Sprintf("got: %+v; expected perc99 <= %v", metrics.ApiCalls[i], latencyThreshold))
		}
		if top > 0 || isBad {
			top--
			prefix := ""
			if isBad {
				prefix = "WARNING "
			}
			klog.Infof("%s: %vTop latency metric: %+v; threshold: %v", a, prefix, metrics.ApiCalls[i], latencyThreshold)
		}
	}

	content, err := util.PrettyPrintJSON(apiCallToPerfData(metrics))
	if err != nil {
		return nil, err
	}
	summary := measurement.CreateSummary(apiResponsivenessMeasurementName, "json", content)
	if len(badMetrics) > 0 {
		return summary, errors.NewMetricViolationError("top latency metric", fmt.Sprintf("there should be no high-latency requests, but: %v", badMetrics))
	}
	return summary, nil
}

func apiserverMetricsReset(c clientset.Interface) error {
	body, err := c.CoreV1().RESTClient().Delete().AbsPath("/metrics").DoRaw()
	if err != nil {
		return err
	}
	if string(body) != "metrics reset\n" {
		return fmt.Errorf("unexpected response: %q", string(body))
	}
	return nil
}

func readLatencyMetrics(c clientset.Interface) (*apiResponsiveness, error) {
	var a apiResponsiveness

	body, err := getMetrics(c)
	if err != nil {
		return nil, err
	}

	samples, err := measurementutil.ExtractMetricSamples(body)
	if err != nil {
		return nil, err
	}

	ignoredResources := sets.NewString("events")
	// TODO: figure out why we're getting non-capitalized proxy and fix this.
	ignoredVerbs := sets.NewString("WATCH", "WATCHLIST", "PROXY", "proxy", "CONNECT")

	for _, sample := range samples {
		// Example line:
		// apiserver_request_latencies_summary{resource="namespaces",verb="LIST",quantile="0.99"} 908
		// apiserver_request_count{resource="pods",verb="LIST",client="kubectl",code="200",contentType="json"} 233
		if sample.Metric[model.MetricNameLabel] != "apiserver_request_latencies_summary" &&
			sample.Metric[model.MetricNameLabel] != "apiserver_request_count" {
			continue
		}

		resource := string(sample.Metric["resource"])
		subresource := string(sample.Metric["subresource"])
		verb := string(sample.Metric["verb"])
		scope := string(sample.Metric["scope"])
		if ignoredResources.Has(resource) || ignoredVerbs.Has(verb) {
			continue
		}

		switch sample.Metric[model.MetricNameLabel] {
		case "apiserver_request_latencies_summary":
			latency := sample.Value
			quantile, err := strconv.ParseFloat(string(sample.Metric[model.QuantileLabel]), 64)
			if err != nil {
				return nil, err
			}
			a.addMetricRequestLatency(resource, subresource, verb, scope, quantile, time.Duration(int64(latency))*time.Microsecond)
		case "apiserver_request_count":
			count := sample.Value
			a.addMetricRequestCount(resource, subresource, verb, scope, int(count))

		}
	}

	return &a, err
}

func getMetrics(c clientset.Interface) (string, error) {
	body, err := c.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw()
	if err != nil {
		return "", err
	}
	return string(body), nil
}

type apiCall struct {
	Resource    string                        `json:"resource"`
	Subresource string                        `json:"subresource"`
	Verb        string                        `json:"verb"`
	Scope       string                        `json:"scope"`
	Latency     measurementutil.LatencyMetric `json:"latency"`
	Count       int                           `json:"count"`
}

type apiResponsiveness struct {
	ApiCalls []apiCall `json:"apicalls"`
}

func (a *apiResponsiveness) Len() int { return len(a.ApiCalls) }
func (a *apiResponsiveness) Swap(i, j int) {
	a.ApiCalls[i], a.ApiCalls[j] = a.ApiCalls[j], a.ApiCalls[i]
}
func (a *apiResponsiveness) Less(i, j int) bool {
	return a.ApiCalls[i].Latency.Perc99 < a.ApiCalls[j].Latency.Perc99
}

// Set request latency for a particular quantile in the apiCall metric entry (creating one if necessary).
// 0 <= quantile <=1 (e.g. 0.95 is 95%tile, 0.5 is median)
// Only 0.5, 0.9 and 0.99 quantiles are supported.
func (a *apiResponsiveness) addMetricRequestLatency(resource, subresource, verb, scope string, quantile float64, latency time.Duration) {
	for i, apicall := range a.ApiCalls {
		if apicall.Resource == resource && apicall.Subresource == subresource && apicall.Verb == verb && apicall.Scope == scope {
			a.ApiCalls[i] = setQuantileAPICall(apicall, quantile, latency)
			return
		}
	}
	apicall := setQuantileAPICall(apiCall{Resource: resource, Subresource: subresource, Verb: verb, Scope: scope}, quantile, latency)
	a.ApiCalls = append(a.ApiCalls, apicall)
}

// Add request count to the apiCall metric entry (creating one if necessary).
func (a *apiResponsiveness) addMetricRequestCount(resource, subresource, verb, scope string, count int) {
	for i, apicall := range a.ApiCalls {
		if apicall.Resource == resource && apicall.Subresource == subresource && apicall.Verb == verb && apicall.Scope == scope {
			a.ApiCalls[i].Count += count
			return
		}
	}
	apicall := apiCall{Resource: resource, Subresource: subresource, Verb: verb, Count: count, Scope: scope}
	a.ApiCalls = append(a.ApiCalls, apicall)
}

// 0 <= quantile <=1 (e.g. 0.95 is 95%tile, 0.5 is median)
// Only 0.5, 0.9 and 0.99 quantiles are supported.
func setQuantileAPICall(apicall apiCall, quantile float64, latency time.Duration) apiCall {
	apicall.Latency.SetQuantile(quantile, latency)
	return apicall
}

// apiCallToPerfData transforms apiResponsiveness to PerfData.
func apiCallToPerfData(apicalls *apiResponsiveness) *measurementutil.PerfData {
	perfData := &measurementutil.PerfData{Version: currentAPICallMetricsVersion}
	for _, apicall := range apicalls.ApiCalls {
		item := measurementutil.DataItem{
			Data: map[string]float64{
				"Perc50": float64(apicall.Latency.Perc50) / 1000000, // us -> ms
				"Perc90": float64(apicall.Latency.Perc90) / 1000000,
				"Perc99": float64(apicall.Latency.Perc99) / 1000000,
			},
			Unit: "ms",
			Labels: map[string]string{
				"Verb":        apicall.Verb,
				"Resource":    apicall.Resource,
				"Subresource": apicall.Subresource,
				"Scope":       apicall.Scope,
				"Count":       fmt.Sprintf("%v", apicall.Count),
			},
		}
		perfData.DataItems = append(perfData.DataItems, item)
	}
	return perfData
}
