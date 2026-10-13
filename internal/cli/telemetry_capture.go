package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A runner's telemetry capture: OTLP JSON lines files per signal, under the
// instance's own telemetry/{role-id}:{instance-id}/ prefix in the stack's
// cache bucket: <signal>.jsonl, plus <signal>-<time>.jsonl segments the
// collector rotated out. The agent uploads them at shutdown, once its
// collector has drained.
var telemetrySignals = []string{"traces", "logs", "metrics"}

type telemetryCaptureObject struct {
	InstanceID string
	Signal     string // one of telemetrySignals
	Name       string // the file name
	Key        string
}

// captureSignal returns the signal of a capture file name, or "". A name
// with a path separator, which would escape the `roc logs --full` archive, is
// not a capture.
func captureSignal(name string) string {
	if strings.ContainsAny(name, `/\`) {
		return ""
	}
	for _, signal := range telemetrySignals {
		if name == signal+".jsonl" || (strings.HasPrefix(name, signal+"-") && strings.HasSuffix(name, ".jsonl")) {
			return signal
		}
	}
	return ""
}

// listTelemetryCapture lists the capture files of the given instances: by
// instance, then signal, rotated segments oldest first and the current file
// last. roleID is the runner instance role's unique ID.
func listTelemetryCapture(ctx context.Context, client metricsS3API, bucket, roleID string, instanceIDs []string) ([]telemetryCaptureObject, error) {
	var objects []telemetryCaptureObject
	listed := make(map[string]struct{}, len(instanceIDs))
	for _, instanceID := range instanceIDs {
		if _, done := listed[instanceID]; done {
			continue
		}
		listed[instanceID] = struct{}{}
		prefix := fmt.Sprintf("telemetry/%s:%s/", roleID, instanceID)
		bySignal := make(map[string][]telemetryCaptureObject)
		paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket),
			Prefix: aws.String(prefix),
		})
		for paginator.HasMorePages() {
			output, err := paginator.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, object := range output.Contents {
				key := aws.ToString(object.Key)
				name := strings.TrimPrefix(key, prefix)
				if signal := captureSignal(name); signal != "" {
					bySignal[signal] = append(bySignal[signal], telemetryCaptureObject{InstanceID: instanceID, Signal: signal, Name: name, Key: key})
				}
			}
		}
		for _, signal := range telemetrySignals {
			// Rotated names carry the rotation time, so they sort oldest first;
			// the current file goes last.
			files := bySignal[signal]
			slices.SortFunc(files, func(a, b telemetryCaptureObject) int {
				aCurrent, bCurrent := a.Name == signal+".jsonl", b.Name == signal+".jsonl"
				if aCurrent != bCurrent {
					if aCurrent {
						return 1
					}
					return -1
				}
				return strings.Compare(a.Name, b.Name)
			})
			objects = append(objects, files...)
		}
	}
	return objects, nil
}
