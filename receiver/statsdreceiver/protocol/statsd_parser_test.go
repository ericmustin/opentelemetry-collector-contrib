// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package protocol

import (
	"errors"
	"testing"

	metricspb "github.com/census-instrumentation/opencensus-proto/gen-go/metrics/v1"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func Test_StatsDParser_Parse(t *testing.T) {
	prevTimeNowFunc := timeNowFunc
	timeNowFunc = func() int64 {
		return 0
	}
	t.Cleanup(
		func() {
			timeNowFunc = prevTimeNowFunc
		},
	)

	tests := []struct {
		name       string
		input      string
		wantMetric *metricspb.Metric
		err        error
	}{
		{
			name:  "empty input string",
			input: "",
			err:   errors.New("invalid message format: "),
		},
		{
			name:  "missing metric value",
			input: "test.metric|c",
			err:   errors.New("invalid <name>:<value> format: test.metric"),
		},
		{
			name:  "empty metric name",
			input: ":42|c",
			err:   errors.New("empty metric name"),
		},
		{
			name:  "empty metric value",
			input: "test.metric:|c",
			err:   errors.New("empty metric value"),
		},
		{
			name:  "integer counter",
			input: "test.metric:42|c",
			wantMetric: testMetric("test.metric",
				metricspb.MetricDescriptor_CUMULATIVE_INT64,
				nil,
				nil,
				&metricspb.Point{
					Timestamp: &timestamppb.Timestamp{
						Seconds: 0,
					},
					Value: &metricspb.Point_Int64Value{
						Int64Value: 42,
					},
				}),
		},
		{
			name:  "gracefully handle float counter value",
			input: "test.metric:42.0|c",
			wantMetric: testMetric("test.metric",
				metricspb.MetricDescriptor_CUMULATIVE_DOUBLE,
				nil,
				nil,
				&metricspb.Point{
					Timestamp: &timestamppb.Timestamp{
						Seconds: 0,
					},
					Value: &metricspb.Point_DoubleValue{
						DoubleValue: 42,
					},
				}),
		},
		{
			name:  "invalid metric value",
			input: "test.metric:42.abc|c",
			err:   errors.New("parse metric value string: 42.abc"),
		},
		{
			name:  "unhandled metric type",
			input: "test.metric:42|unhandled_type",
			err:   errors.New("unsupported metric type: unhandled_type"),
		},
		{
			name:  "counter metric with sample rate and tags",
			input: "test.metric:42|c|@0.1|#key:value",
			wantMetric: testMetric("test.metric",
				metricspb.MetricDescriptor_CUMULATIVE_INT64,
				[]*metricspb.LabelKey{
					{
						Key: "key",
					},
				},
				[]*metricspb.LabelValue{
					{
						Value:    "value",
						HasValue: true,
					},
				},
				&metricspb.Point{
					Timestamp: &timestamppb.Timestamp{
						Seconds: 0,
					},
					Value: &metricspb.Point_Int64Value{
						Int64Value: 42,
					},
				}),
		},
		{
			name:  "double gauge metric",
			input: "test.gauge:42.0|g|@0.1|#key:value",
			wantMetric: testMetric("test.gauge",
				metricspb.MetricDescriptor_GAUGE_DOUBLE,
				[]*metricspb.LabelKey{
					{
						Key: "key",
					},
				},
				[]*metricspb.LabelValue{
					{
						Value:    "value",
						HasValue: true,
					},
				},
				&metricspb.Point{
					Timestamp: &timestamppb.Timestamp{
						Seconds: 0,
					},
					Value: &metricspb.Point_DoubleValue{
						DoubleValue: 42,
					},
				}),
		},
		{
			name:  "int gauge metric",
			input: "test.gauge:42|g|@0.1|#key:value",
			wantMetric: testMetric("test.gauge",
				metricspb.MetricDescriptor_GAUGE_INT64,
				[]*metricspb.LabelKey{
					{
						Key: "key",
					},
				},
				[]*metricspb.LabelValue{
					{
						Value:    "value",
						HasValue: true,
					},
				},
				&metricspb.Point{
					Timestamp: &timestamppb.Timestamp{
						Seconds: 0,
					},
					Value: &metricspb.Point_Int64Value{
						Int64Value: 42,
					},
				}),
		},
		{
			name:  "invalid sample rate value",
			input: "test.metric:42|c|@1.0a",
			err:   errors.New("parse sample rate: 1.0a"),
		},
		{
			name:  "invalid tag format",
			input: "test.metric:42|c|#key1",
			err:   errors.New("invalid tag format: [key1]"),
		},
		{
			name:  "unrecognized message part",
			input: "test.metric:42|c|$extra",
			err:   errors.New("unrecognized message part: $extra"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &StatsDParser{}

			got, err := p.Parse(tt.input)

			if tt.err != nil {
				assert.Equal(t, err, tt.err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, got, tt.wantMetric)
			}
		})
	}
}

func testMetric(metricName string,
	metricType metricspb.MetricDescriptor_Type,
	lableKeys []*metricspb.LabelKey,
	labelValues []*metricspb.LabelValue,
	point *metricspb.Point) *metricspb.Metric {
	return &metricspb.Metric{
		MetricDescriptor: &metricspb.MetricDescriptor{
			Name:      metricName,
			Type:      metricType,
			LabelKeys: lableKeys,
		},
		Timeseries: []*metricspb.TimeSeries{
			{
				LabelValues: labelValues,
				Points: []*metricspb.Point{
					point,
				},
			},
		},
	}
}
