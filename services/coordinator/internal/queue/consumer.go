package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	streamName    = "APK_EVENTS"
	consumerName  = "coordinator"
	subjectAll    = "apk.>"
	subjectReady  = "apk.ingested"
	subjectFailed = "apk.ingestion.failed"
	maxDeliver    = 5
)

// IngestedMessage is published by the ingestion service when an APK is ready.
type IngestedMessage struct {
	JobID       string `json:"job_id"`
	APKPath     string `json:"apk_path"`
	PackageName string `json:"package_name"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	SHA256      string `json:"sha256"`
	SubmittedAt string `json:"submitted_at"` // RFC3339
}

// FailedMessage is published by the ingestion service when ingestion fails.
type FailedMessage struct {
	JobID string `json:"job_id"`
	Error string `json:"error"`
}

// MessageHandler handles APK pipeline events.
type MessageHandler interface {
	HandleIngested(ctx context.Context, msg *IngestedMessage) error
	HandleIngestionFailed(ctx context.Context, msg *FailedMessage) error
}

// Consumer subscribes to APK_EVENTS JetStream and dispatches messages.
type Consumer struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	handler MessageHandler
}

// NewConsumer connects to NATS, ensures the stream and durable consumer exist,
// and returns a ready Consumer.
func NewConsumer(natsURL string, handler MessageHandler) (*Consumer, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, fmt.Errorf("connect to nats: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create jetstream context: %w", err)
	}

	ctx := context.Background()

	// Ensure stream exists
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subjectAll},
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create/update stream: %w", err)
	}

	return &Consumer{nc: nc, js: js, handler: handler}, nil
}

// Run starts consuming messages and blocks until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	// Durable consumer for apk.ingested with MaxDeliver=5
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:        consumerName,
		FilterSubject:  subjectReady,
		MaxDeliver:     maxDeliver,
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckWait:        5 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("create durable consumer: %w", err)
	}

	// Ephemeral consumer for apk.ingestion.failed
	failConsumer, err := c.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:       consumerName + "-failed",
		FilterSubject: subjectFailed,
		MaxDeliver:    maxDeliver,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       1 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("create failed consumer: %w", err)
	}

	msgs, err := consumer.Messages()
	if err != nil {
		return fmt.Errorf("get messages iterator: %w", err)
	}
	defer msgs.Stop()

	failMsgs, err := failConsumer.Messages()
	if err != nil {
		return fmt.Errorf("get failed messages iterator: %w", err)
	}
	defer failMsgs.Stop()

	// Fan-in both iterators via goroutines feeding a channel
	type envelope struct {
		msg     jetstream.Msg
		subject string
	}
	ch := make(chan envelope, 64)

	go func() {
		for {
			msg, err := msgs.Next()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("consumer: error reading ingested message: %v", err)
				continue
			}
			ch <- envelope{msg: msg, subject: subjectReady}
		}
	}()

	go func() {
		for {
			msg, err := failMsgs.Next()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("consumer: error reading failed message: %v", err)
				continue
			}
			ch <- envelope{msg: msg, subject: subjectFailed}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case env := <-ch:
			switch env.subject {
			case subjectReady:
				c.handleIngested(ctx, env.msg)
			case subjectFailed:
				c.handleFailed(ctx, env.msg)
			}
		}
	}
}

func (c *Consumer) handleIngested(ctx context.Context, msg jetstream.Msg) {
	var m IngestedMessage
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		log.Printf("consumer: failed to unmarshal IngestedMessage: %v", err)
		// Bad message — ack to avoid infinite redelivery of unparseable data
		_ = msg.Ack()
		return
	}

	if err := c.handler.HandleIngested(ctx, &m); err != nil {
		log.Printf("consumer: HandleIngested error for job %s: %v", m.JobID, err)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

func (c *Consumer) handleFailed(ctx context.Context, msg jetstream.Msg) {
	var m FailedMessage
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		log.Printf("consumer: failed to unmarshal FailedMessage: %v", err)
		_ = msg.Ack()
		return
	}

	if err := c.handler.HandleIngestionFailed(ctx, &m); err != nil {
		log.Printf("consumer: HandleIngestionFailed error for job %s: %v", m.JobID, err)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

// Close shuts down the NATS connection.
func (c *Consumer) Close() {
	c.nc.Close()
}

