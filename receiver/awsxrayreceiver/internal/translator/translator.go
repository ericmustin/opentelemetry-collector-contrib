// Copyright The OpenTelemetry Authors
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

package translator

import (
	"encoding/json"

	otlptrace "github.com/open-telemetry/opentelemetry-proto/gen/go/trace/v1"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/translator/conventions"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/awsxray"
)

const (
	// just a guess to avoid too many memory re-allocation
	initAttrCapacity = 15
)

// TODO: It might be nice to consolidate the `fromPdata` in x-ray exporter and
// `toPdata` in this receiver to a common package later

// ToTraces converts X-Ray segment (and its subsegments) to an OT ResourceSpans.
func ToTraces(rawSeg []byte) (*pdata.Traces, int, error) {
	var seg awsxray.Segment
	err := json.Unmarshal(rawSeg, &seg)
	if err != nil {
		// return 1 as total segment (&subsegments) count
		// because we can't parse the body the UDP packet.
		return nil, 1, err
	}
	count := totalSegmentsCount(seg)

	err = seg.Validate()
	if err != nil {
		return nil, count, err
	}

	traceData := pdata.NewTraces()
	rspanSlice := traceData.ResourceSpans()
	// ## allocate a new otlptrace.ResourceSpans for the segment document
	// (potentially with embedded subsegments)
	rspanSlice.Resize(1)      // initialize a new empty pdata.ResourceSpans
	rspan := rspanSlice.At(0) // retrieve the empty pdata.ResourceSpans we just created

	// ## initialize the fields in a ResourceSpans
	resource := rspan.Resource()
	resource.InitEmpty()
	// each segment (with its subsegments) is generated by one instrument
	// library so only allocate one `InstrumentationLibrarySpans` in the
	// `InstrumentationLibrarySpansSlice`.
	rspan.InstrumentationLibrarySpans().Resize(1)
	ils := rspan.InstrumentationLibrarySpans().At(0)
	ils.Spans().Resize(count)
	spans := ils.Spans()

	// populating global attributes shared among segment and embedded subsegment(s)
	populateResource(&seg, &resource)

	// recursively traverse segment and embedded subsegments
	// to populate the spans. We also need to pass in the
	// TraceID of the root segment in because embedded subsegments
	// do not have that information, but it's needed after we flatten
	// the embedded subsegment to generate independent child spans.
	_, _, err = segToSpans(seg, seg.TraceID, nil, &spans, 0)
	if err != nil {
		return nil, count, err
	}

	return &traceData, count, nil
}

func segToSpans(seg awsxray.Segment,
	traceID, parentID *string,
	spans *pdata.SpanSlice, startingIndex int) (int, *pdata.Span, error) {

	span := spans.At(startingIndex)

	err := populateSpan(&seg, traceID, parentID, &span)
	if err != nil {
		return 0, nil, err
	}

	startingIndexForSubsegment := 1 + startingIndex
	var populatedChildSpan *pdata.Span
	for _, s := range seg.Subsegments {
		startingIndexForSubsegment, populatedChildSpan, err = segToSpans(s,
			traceID, seg.ID,
			spans, startingIndexForSubsegment)
		if err != nil {
			return 0, nil, err
		}

		if seg.Cause != nil &&
			populatedChildSpan.Status().Code() != pdata.StatusCode(otlptrace.Status_Ok) {
			// if seg.Cause is not nil, then one of the subsegments must contain a
			// HTTP error code. Also, span.Status().Code() is already
			// set to `otlptrace.Status_UnknownError` by `addCause()` in
			// `populateSpan()` above, so here we are just trying to figure out
			// whether we can get an even more specific error code.

			if span.Status().Code() == pdata.StatusCode(otlptrace.Status_UnknownError) {
				// update the error code to a possibly more specific code
				span.Status().SetCode(populatedChildSpan.Status().Code())
			}
		}
	}

	return startingIndexForSubsegment, &span, nil
}

func populateSpan(
	seg *awsxray.Segment,
	traceID, parentID *string,
	span *pdata.Span) error {

	span.Status().InitEmpty() // by default this sets the code to `Status_Ok`
	attrs := span.Attributes()
	attrs.InitEmptyWithCapacity(initAttrCapacity)

	err := addNameAndNamespace(seg, span)
	if err != nil {
		return err
	}

	if seg.TraceID == nil {
		// if seg.TraceID is nil, then `seg` must be an embedded subsegment.
		span.SetTraceID(pdata.TraceID([]byte(*traceID)))
	} else {
		span.SetTraceID(pdata.TraceID([]byte(*seg.TraceID)))
	}
	span.SetSpanID(pdata.SpanID([]byte(*seg.ID)))
	addParentSpanID(seg, parentID, span)
	addStartTime(seg.StartTime, span)

	addEndTime(seg.EndTime, span)
	addBool(seg.InProgress, awsxray.AWSXRayInProgressAttribute, &attrs)
	addString(seg.User, conventions.AttributeEnduserID, &attrs)

	addHTTP(seg, span)
	addCause(seg, span)
	addAWSToSpan(seg.AWS, &attrs)
	err = addSQLToSpan(seg.SQL, &attrs)
	if err != nil {
		return err
	}

	addBool(seg.Traced, awsxray.AWSXRayTracedAttribute, &attrs)

	addAnnotations(seg.Annotations, &attrs)
	addMetadata(seg.Metadata, &attrs)

	return nil
}

func populateResource(seg *awsxray.Segment, rs *pdata.Resource) {
	// allocate a new attribute map within the Resource in the pdata.ResourceSpans allocated above
	attrs := rs.Attributes()
	attrs.InitEmptyWithCapacity(initAttrCapacity)

	addAWSToResource(seg.AWS, &attrs)
	addSdkToResource(seg, &attrs)
	if seg.Service != nil {
		addString(
			seg.Service.Version,
			conventions.AttributeServiceVersion,
			&attrs)
	}

	addString(seg.ResourceARN, awsxray.AWSXRayResourceARNAttribute, &attrs)
}

func totalSegmentsCount(seg awsxray.Segment) int {
	subsegmentCount := 0
	for _, s := range seg.Subsegments {
		subsegmentCount += totalSegmentsCount(s)
	}

	return 1 + subsegmentCount
}
