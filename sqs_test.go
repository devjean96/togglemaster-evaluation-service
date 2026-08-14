package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/sqs"
)

type mockSQSClient struct {
	input *sqs.SendMessageInput
	err   error
}

func (m *mockSQSClient) SendMessage(input *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
	m.input = input
	return &sqs.SendMessageOutput{}, m.err
}

func TestSendEvaluationEventIsOptional(t *testing.T) {
	app := &App{}
	app.sendEvaluationEvent("user-1", "checkout", true)
}

func TestSendEvaluationEvent(t *testing.T) {
	client := &mockSQSClient{}
	app := &App{SqsSvc: client, SqsQueueURL: "queue-url"}

	app.sendEvaluationEvent("user-1", "checkout", true)

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

	app.sendEvaluationEvent("user-1", "checkout", false)

	if client.input == nil {
		t.Fatal("expected an SQS send attempt")
	}
}
