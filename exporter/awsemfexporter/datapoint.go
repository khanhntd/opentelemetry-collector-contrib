// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package awsemfexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/awsemfexporter"

import (
	"fmt"
	"strconv"
	"time"

	aws "github.com/open-telemetry/opentelemetry-collector-contrib/internal/aws/metrics"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

var deltaMetricCalculator = aws.NewFloat64DeltaCalculator()
var summaryMetricCalculator = aws.NewMetricCalculator(calculateSummaryDelta)

func calculateSummaryDelta(prev *aws.MetricValue, val interface{}, timestampMs time.Time) (interface{}, bool) {
	metricEntry := val.(summaryMetricEntry)
	summaryDelta := metricEntry.sum
	countDelta := metricEntry.count
	if prev != nil {
		prevSummaryEntry := prev.RawValue.(summaryMetricEntry)
		summaryDelta = metricEntry.sum - prevSummaryEntry.sum
		countDelta = metricEntry.count - prevSummaryEntry.count
	} else {
		return summaryMetricEntry{summaryDelta, countDelta}, false
	}
	return summaryMetricEntry{summaryDelta, countDelta}, true
}

// dataPoint represents a processed metric data point
type dataPoint struct {
	name        string
	value       interface{}
	labels      map[string]string
	timestampMs int64
}

// dataPointSlice is a wrapper interface for:
//   - pmetric.NumberDataPointSlice
//   - pmetric.HistogramDataPointSlice
//   - pmetric.SummaryDataPointSlice
type dataPointSlice interface {
	Len() int
	// At gets the adjusted datapoint from the DataPointSlice at i-th index.
	// dataPoint: the adjusted data point
	// retained: indicates whether the data point is valid for further process
	// NOTE: It is an expensive call as it calculates the metric value.
	CalculateDeltaDatapoints(i int, instrumentationLibraryName string) (dataPoint []dataPoint, retained bool)
}

// deltaMetricMetadata contains the metadata required to perform rate/delta calculation
type deltaMetricMetadata struct {
	adjustToDelta bool
	metricName    string
	namespace     string
	logGroup      string
	logStream     string
}

// numberDataPointSlice is a wrapper for pmetric.NumberDataPointSlice
type numberDataPointSlice struct {
	deltaMetricMetadata
	pmetric.NumberDataPointSlice
}

// histogramDataPointSlice is a wrapper for pmetric.HistogramDataPointSlice
type histogramDataPointSlice struct {
	deltaMetricMetadata
	pmetric.HistogramDataPointSlice
}

// summaryDataPointSlice is a wrapper for pmetric.SummaryDataPointSlice
type summaryDataPointSlice struct {
	deltaMetricMetadata
	pmetric.SummaryDataPointSlice
}

type summaryMetricEntry struct {
	sum   float64
	count uint64
}

// At retrieves the NumberDataPoint at the given index and performs rate/delta calculation if necessary.
func (dps numberDataPointSlice) CalculateDeltaDatapoints(i int, instrumentationLibraryName string) ([]dataPoint, bool) {
	metric := dps.NumberDataPointSlice.At(i)
	labels := createLabels(metric.Attributes(), instrumentationLibraryName)
	timestampMs := unixNanoToMilliseconds(metric.Timestamp())

	var metricVal float64
	switch metric.ValueType() {
	case pmetric.NumberDataPointValueTypeDouble:
		metricVal = metric.DoubleValue()
	case pmetric.NumberDataPointValueTypeInt:
		metricVal = float64(metric.IntValue())
	}

	var retained bool = true
	var datapoints []dataPoint = []dataPoint{}

	if dps.adjustToDelta {
		var deltaVal interface{}
		deltaVal, retained = deltaMetricCalculator.Calculate(aws.NewKey(dps.deltaMetricMetadata, labels), metricVal, metric.Timestamp().AsTime())
		if !retained {
			return datapoints, retained
		}
		// It should not happen in practice that the previous metric value is smaller than the current one.
		// If it happens, we assume that the metric is reset for some reason.
		if deltaVal.(float64) >= 0 {
			metricVal = deltaVal.(float64)
		}
	}

	datapoints = append(datapoints, dataPoint{name: dps.metricName, value: metricVal, labels: labels, timestampMs: timestampMs})

	return datapoints, retained
}

// At retrieves the HistogramDataPoint at the given index.
func (dps histogramDataPointSlice) CalculateDeltaDatapoints(i int, instrumentationLibraryName string) ([]dataPoint, bool) {
	metric := dps.HistogramDataPointSlice.At(i)
	labels := createLabels(metric.Attributes(), instrumentationLibraryName)
	timestamp := unixNanoToMilliseconds(metric.Timestamp())

	return []dataPoint{dataPoint{
		name: dps.metricName,
		value: &cWMetricStats{
			Count: metric.Count(),
			Sum:   metric.Sum(),
			Max:   metric.Max(),
			Min:   metric.Min(),
		},
		labels:      labels,
		timestampMs: timestamp,
	}}, true
}

// At retrieves the SummaryDataPoint at the given index.
func (dps summaryDataPointSlice) CalculateDeltaDatapoints(i int, instrumentationLibraryName string) ([]dataPoint, bool) {
	metric := dps.SummaryDataPointSlice.At(i)
	labels := createLabels(metric.Attributes(), instrumentationLibraryName)
	timestampMs := unixNanoToMilliseconds(metric.Timestamp())

	sum := metric.Sum()
	count := metric.Count()
	var retained bool = true
	var datapoints []dataPoint = []dataPoint{}

	if dps.adjustToDelta {
		var delta interface{}
		delta, retained = summaryMetricCalculator.Calculate(aws.NewKey(dps.deltaMetricMetadata, labels), summaryMetricEntry{sum, count}, metric.Timestamp().AsTime())
		if !retained {
			return datapoints, retained
		}
		summaryMetricDelta := delta.(summaryMetricEntry)
		sum = summaryMetricDelta.sum
		count = summaryMetricDelta.count
	}

	if false {
		metricVal := &cWMetricStats{
			Count: count,
			Sum:   sum,
		}
		if quantileValues := metric.QuantileValues(); quantileValues.Len() > 0 {
			metricVal.Min = quantileValues.At(0).Value()
			metricVal.Max = quantileValues.At(quantileValues.Len() - 1).Value()
		}
		datapoints = append(datapoints, dataPoint{value: metricVal, labels: labels, timestampMs: timestampMs})
	} else {
		values := metric.QuantileValues()
		datapoints = append(datapoints, dataPoint{name: fmt.Sprint(dps.metricName, "_count"), value: count, labels: labels, timestampMs: timestampMs})
		datapoints = append(datapoints, dataPoint{name: fmt.Sprint(dps.metricName, "_sum"), value: sum, labels: labels, timestampMs: timestampMs})
		for i := 0; i < values.Len(); i++ {
			value := values.At(i)
			labels["quantile"] = strconv.FormatFloat(value.Quantile(), 'g', -1, 64)
			datapoints = append(datapoints, dataPoint{name: dps.metricName, value: value.Value(), labels: labels, timestampMs: timestampMs})
		}
	}

	return datapoints, retained
}

// createLabels converts OTel AttributesMap attributes to a map
// and optionally adds in the OTel instrumentation library name
func createLabels(attributes pcommon.Map, instrLibName string) map[string]string {
	labels := make(map[string]string, attributes.Len()+1)
	attributes.Range(func(k string, v pcommon.Value) bool {
		labels[k] = v.AsString()
		return true
	})

	// Add OTel instrumentation lib name as an additional label if it is defined
	if instrLibName != "" {
		labels[oTellibDimensionKey] = instrLibName
	}

	return labels
}

// getDataPoints retrieves data points from OT Metric.
func getDataPoints(pmd pmetric.Metric, metadata cWMetricMetadata, logger *zap.Logger) dataPointSlice {
	metricMetadata := deltaMetricMetadata{
		false,
		pmd.Name(),
		metadata.namespace,
		metadata.logGroup,
		metadata.logStream,
	}

	var dps dataPointSlice = nil
	switch pmd.Type() {
	case pmetric.MetricTypeGauge:
		metric := pmd.Gauge()
		dps = numberDataPointSlice{
			metricMetadata,
			metric.DataPoints(),
		}
	case pmetric.MetricTypeSum:
		metric := pmd.Sum()
		metricMetadata.adjustToDelta = metric.AggregationTemporality() == pmetric.AggregationTemporalityCumulative
		dps = numberDataPointSlice{
			metricMetadata,
			metric.DataPoints(),
		}
	case pmetric.MetricTypeHistogram:
		metric := pmd.Histogram()
		dps = histogramDataPointSlice{
			metricMetadata,
			metric.DataPoints(),
		}
	case pmetric.MetricTypeSummary:
		metric := pmd.Summary()
		// For summaries coming from the prometheus receiver, the sum and count are cumulative, whereas for summaries
		// coming from other sources, e.g. SDK, the sum and count are delta by being accumulated and reset periodically.
		// In order to ensure metrics are sent as deltas, we check the receiver attribute (which can be injected by
		// attribute processor) from resource metrics. If it exists, and equals to prometheus, the sum and count will be
		// converted.
		// For more information: https://github.com/open-telemetry/opentelemetry-collector/blob/main/receiver/prometheusreceiver/DESIGN.md#summary
		metricMetadata.adjustToDelta = metadata.receiver == prometheusReceiver
		dps = summaryDataPointSlice{
			metricMetadata,
			metric.DataPoints(),
		}
	default:
		logger.Warn("Unhandled metric data type.",
			zap.String("DataType", pmd.Type().String()),
			zap.String("Name", pmd.Name()),
			zap.String("Unit", pmd.Unit()),
		)
	}

	return dps
}
