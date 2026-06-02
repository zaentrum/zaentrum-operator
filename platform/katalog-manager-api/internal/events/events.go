// Package events is the Kafka producer for the management plane. Processing
// work is never invoked synchronously: the service emits a task event on a
// stube.processing.task.* topic and the relevant worker (transcoder,
// packager, metadata enricher) consumes it. This keeps the management plane
// decoupled from worker availability and lets work fan out horizontally.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/IBM/sarama"
)

// Processing task topics. The naming follows the platform convention
// stube.{domain}.{event}; here the domain is `processing.task` and the leaf
// is the kind of work requested.
const (
	TopicTranscode = "stube.processing.task.transcode"
	TopicPackage   = "stube.processing.task.package"
	TopicEnrich    = "stube.processing.task.enrich"
)

// Task is the JSON body of a processing task event. itemId is the catalog
// item the work targets; kind echoes the topic leaf for consumers that
// subscribe to a wildcard. RequestedAt lets a worker compute queue latency.
type Task struct {
	ItemID      string `json:"itemId"`
	Kind        string `json:"kind"`
	RequestedAt string `json:"requestedAt"`
	// Source is informational provenance — always this service.
	Source string `json:"source"`
}

// Producer wraps a sarama.SyncProducer. A nil *Producer (scaffold mode, no
// brokers configured) makes Publish a logged no-op so the service still
// functions for first-run setup and catalog edits before the broker exists.
type Producer struct {
	p sarama.SyncProducer
}

// NewProducer connects a sync producer to the given broker list. An empty
// slice yields a nil-backed Producer (scaffold mode). Errors from the broker
// dial are returned so the caller can log and continue.
func NewProducer(brokers []string) (*Producer, error) {
	if len(brokers) == 0 {
		slog.Warn("KAFKA_BROKERS empty; events producer in scaffold mode (publishes are no-ops)")
		return &Producer{}, nil
	}
	cfg := sarama.NewConfig()
	cfg.Producer.Return.Successes = true
	cfg.Producer.RequiredAcks = sarama.WaitForAll
	cfg.Producer.Retry.Max = 5
	cfg.Producer.Idempotent = true
	cfg.Net.MaxOpenRequests = 1
	cfg.ClientID = "katalog-manager-api"
	p, err := sarama.NewSyncProducer(brokers, cfg)
	if err != nil {
		return nil, fmt.Errorf("kafka producer: %w", err)
	}
	return &Producer{p: p}, nil
}

// PublishTask emits a processing task for itemID on the given topic. The
// message key is the item id so all work for one item lands on the same
// partition (preserving per-item ordering across transcode → package).
func (p *Producer) PublishTask(ctx context.Context, topic, itemID string) error {
	kind := topicLeaf(topic)
	body := Task{
		ItemID:      itemID,
		Kind:        kind,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
		Source:      "katalog-manager-api",
	}
	value, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	if p == nil || p.p == nil {
		slog.Info("events publish (scaffold no-op)", "topic", topic, "itemId", itemID)
		return nil
	}
	_, _, err = p.p.SendMessage(&sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder(itemID),
		Value: sarama.ByteEncoder(value),
	})
	if err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}
	slog.Info("published processing task", "topic", topic, "itemId", itemID)
	return nil
}

// Ready reports whether the producer is backed by a live broker connection.
// Used by /api/manage/setup/status to report the `kafka` health check.
func (p *Producer) Ready() bool {
	return p != nil && p.p != nil
}

// Close flushes and releases the producer.
func (p *Producer) Close() error {
	if p == nil || p.p == nil {
		return nil
	}
	return p.p.Close()
}

// topicLeaf returns the trailing segment of a dotted topic, e.g.
// "transcode" for "stube.processing.task.transcode". Used as the task kind.
func topicLeaf(topic string) string {
	for i := len(topic) - 1; i >= 0; i-- {
		if topic[i] == '.' {
			return topic[i+1:]
		}
	}
	return topic
}
