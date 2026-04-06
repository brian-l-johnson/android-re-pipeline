package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
)

const (
	streamName    = "APK_EVENTS"
	subjectPrefix = "apk.>"

	subjectIngested = "apk.ingested"
	subjectFailed   = "apk.ingestion.failed"
)

// IngestedMessage is published to apk.ingested when an APK has been written to disk.
type IngestedMessage struct {
	JobID       string `json:"job_id"`
	APKPath     string `json:"apk_path"`
	PackageName string `json:"package_name"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	SHA256      string `json:"sha256"`
	SubmittedAt string `json:"submitted_at"` // RFC3339
}

// FailedMessage is published to apk.ingestion.failed when ingestion fails.
type FailedMessage struct {
	JobID string `json:"job_id"`
	Error string `json:"error"`
}

// Publisher wraps a NATS connection and JetStream context.
type Publisher struct {
	conn *nats.Conn
	js   nats.JetStreamContext
}

// NewPublisher connects to NATS, ensures the APK_EVENTS stream exists, and
// returns a ready Publisher.
func NewPublisher(natsURL string) (*Publisher, error) {
	conn, err := nats.Connect(natsURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", natsURL, err)
	}

	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("getting JetStream context: %w", err)
	}

	_, err = js.StreamInfo(streamName)
	if err != nil {
		// Stream does not exist — create it.
		_, err = js.AddStream(&nats.StreamConfig{
			Name:      streamName,
			Subjects:  []string{subjectPrefix},
			Retention: nats.WorkQueuePolicy,
		})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("creating stream %s: %w", streamName, err)
		}
	}

	return &Publisher{conn: conn, js: js}, nil
}

// Close drains and closes the underlying NATS connection.
func (p *Publisher) Close() {
	p.conn.Drain() //nolint:errcheck
}

// PublishIngested publishes msg to apk.ingested.
func (p *Publisher) PublishIngested(_ context.Context, msg *IngestedMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshalling IngestedMessage: %w", err)
	}
	if _, err := p.js.Publish(subjectIngested, data); err != nil {
		return fmt.Errorf("publishing to %s: %w", subjectIngested, err)
	}
	return nil
}

// PublishFailed publishes msg to apk.ingestion.failed.
func (p *Publisher) PublishFailed(_ context.Context, msg *FailedMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshalling FailedMessage: %w", err)
	}
	if _, err := p.js.Publish(subjectFailed, data); err != nil {
		return fmt.Errorf("publishing to %s: %w", subjectFailed, err)
	}
	return nil
}
