package main

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestTelemetryResourceMergesWithoutSchemaConflict(t *testing.T) {
	res, err := telemetryResource("evaluation-service")
	if err != nil {
		t.Fatalf("merge telemetry resource: %v", err)
	}

	value, ok := res.Set().Value(attribute.Key("service.name"))
	if !ok || value.AsString() != "evaluation-service" {
		t.Fatalf("unexpected service.name: %q", value.AsString())
	}
}
