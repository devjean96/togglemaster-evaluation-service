package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/sqs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type mockSQSClient struct {
	input *sqs.SendMessageInput
	err   error
}

func (m *mockSQSClient) SendMessageWithContext(_ aws.Context, input *sqs.SendMessageInput, _ ...request.Option) (*sqs.SendMessageOutput, error) {
	m.input = input
	return &sqs.SendMessageOutput{}, m.err
}

func TestSendEvaluationEventIsOptional(t *testing.T) {
	app := &App{}
	app.sendEvaluationEvent(context.Background(), "user-1", "checkout", true)
}

func TestSendEvaluationEvent(t *testing.T) {
	client := &mockSQSClient{}
	app := &App{SqsSvc: client, SqsQueueURL: "queue-url"}

	app.sendEvaluationEvent(context.Background(), "user-1", "checkout", true)

	if client.input == nil || *client.input.QueueUrl != "queue-url" {
		t.Fatalf("unexpected SQS input: %+v", client.input)
	}
	var event EvaluationEvent
	if err := json.Unmarshal([]byte(*client.input.MessageBody), &event); err != nil {
		t.Fatalf("decoding event: %v", err)
	}
	if event.UserID != "user-1" || event.FlagName != "checkout" || !event.Result || event.Timestamp.IsZero() {
		t.Fatalf("unexpected event: %+v", event)
	}
}

func TestSendEvaluationEventHandlesSQSError(t *testing.T) {
	client := &mockSQSClient{err: errors.New("SQS unavailable")}
	app := &App{SqsSvc: client, SqsQueueURL: "queue-url"}

	app.sendEvaluationEvent(context.Background(), "user-1", "checkout", false)

	if client.input == nil {
		t.Fatal("expected an SQS send attempt")
	}
}

func TestSendEvaluationEventPropagatesTraceContext(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	client := &mockSQSClient{}
	app := &App{SqsSvc: client, SqsQueueURL: "queue-url"}

	app.sendEvaluationEvent(ctx, "user-1", "checkout", true)
	parent.End()

	traceparent := client.input.MessageAttributes["traceparent"]
	if traceparent == nil || traceparent.StringValue == nil || *traceparent.StringValue == "" {
		t.Fatalf("expected traceparent message attribute, got %+v", client.input.MessageAttributes)
	}
}
