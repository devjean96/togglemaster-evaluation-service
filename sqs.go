package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/sqs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Evento que será enviado para a fila
type EvaluationEvent struct {
	UserID    string    `json:"user_id"`
	FlagName  string    `json:"flag_name"`
	Result    bool      `json:"result"`
	Timestamp time.Time `json:"timestamp"`
}

// SQSClient contém apenas a operação usada pela aplicação e facilita testes unitários.
type SQSClient interface {
	SendMessageWithContext(aws.Context, *sqs.SendMessageInput, ...request.Option) (*sqs.SendMessageOutput, error)
}

// sendEvaluationEvent envia um evento para a fila SQS
func (a *App) sendEvaluationEvent(ctx context.Context, userID, flagName string, result bool) {
	ctx, span := evaluationTracer.Start(ctx, "sqs.publish_evaluation", trace.WithSpanKind(trace.SpanKindProducer))
	defer span.End()
	span.SetAttributes(
		attribute.String("messaging.system", "aws_sqs"),
		attribute.String("messaging.operation.name", "send"),
		attribute.String("feature_flag.name", flagName),
	)

	// Se a URL da fila não foi configurada, apenas loga localmente e sai.
	if a.SqsSvc == nil || a.SqsQueueURL == "" {
		log.Printf("[SQS_DISABLED] Evento para flag '%s' (resultado: %t)", flagName, result)
		return
	}

	event := EvaluationEvent{
		UserID:    userID,
		FlagName:  flagName,
		Result:    result,
		Timestamp: time.Now().UTC(),
	}

	body, err := json.Marshal(event)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "event serialization failed")
		log.Printf("Erro ao serializar evento SQS: %v", err)
		return
	}

	// Envia a mensagem
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	messageAttributes := make(map[string]*sqs.MessageAttributeValue, 2)
	for _, key := range []string{"traceparent", "tracestate"} {
		if value := carrier.Get(key); value != "" {
			messageAttributes[key] = &sqs.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(value),
			}
		}
	}

	_, err = a.SqsSvc.SendMessageWithContext(ctx, &sqs.SendMessageInput{
		MessageBody:       aws.String(string(body)),
		MessageAttributes: messageAttributes,
		QueueUrl:          aws.String(a.SqsQueueURL),
	})

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "SQS publish failed")
		a.recordSQSPublishFailure(ctx)
		log.Printf("Erro ao enviar mensagem para SQS: %v", err)
	} else {
		log.Printf("Evento de avaliação enviado para SQS (Flag: %s)", flagName)
	}
}
