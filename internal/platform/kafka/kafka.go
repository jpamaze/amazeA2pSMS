// Package kafka defines topic names and client helpers.
//
// Blueprint rule 1: a customer is acknowledged only after the message is in
// Kafka with acks=all. NewProducer enforces that setting.
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Topic names. Vendor route topics are RouteTopic(vendorID).
const (
	TopicSubmit        = "sms.submit"         // domain.Message accepted by an edge, waiting for routing
	TopicStatus        = "sms.status"         // domain.StatusEvent from router and connectors
	TopicDLR           = "sms.dlr"            // domain.DLR journaled by connectors before deliver_sm_resp
	TopicMO            = "sms.mo"             // domain.MO from vendors
	TopicBillingEvents = "sms.billing.events" // refunds and adjustments (later phases)
	TopicConfigChanges = "sms.config.changes" // cache invalidation for in-memory config
	routeTopicPrefix   = "sms.route."
)

// RouteTopic is the per-vendor queue a connector consumes (domain.Message with Route set).
func RouteTopic(vendorID string) string { return routeTopicPrefix + vendorID }

// AllTopics lists every platform topic, including route topics for vendors.
func AllTopics(vendorIDs []string) []string {
	out := []string{TopicSubmit, TopicStatus, TopicDLR, TopicMO, TopicBillingEvents, TopicConfigChanges}
	for _, v := range vendorIDs {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, RouteTopic(v))
		}
	}
	return out
}

// SubmitKey builds the idempotency key for a customer submit.
func SubmitKey(accountID, clientRef string) string {
	return fmt.Sprintf("submit:%s:%s", accountID, clientRef)
}

// DLRKey builds the idempotency key for a vendor DLR.
func DLRKey(vendorID, vendorMsgID string) string {
	return fmt.Sprintf("dlr:%s:%s", vendorID, vendorMsgID)
}

// Record marshals v as JSON into a record for topic, keyed by key.
func Record(topic, key string, v any) (*kgo.Record, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("kafka: marshal for %s: %w", topic, err)
	}
	return &kgo.Record{Topic: topic, Key: []byte(key), Value: b}, nil
}

// ProduceJSON writes one or more records and waits for acks=all.
func ProduceJSON(ctx context.Context, cl *kgo.Client, recs ...*kgo.Record) error {
	if len(recs) == 0 {
		return nil
	}
	return cl.ProduceSync(ctx, recs...).FirstErr()
}

// NewProducer returns a client tuned for durable, idempotent produce.
func NewProducer(brokers []string, opts ...kgo.Opt) (*kgo.Client, error) {
	base := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.ZstdCompression()),
		kgo.ProducerLinger(time.Millisecond),
		kgo.AllowAutoTopicCreation(),
	}
	return kgo.NewClient(append(base, opts...)...)
}

// NewConsumer returns a group consumer that commits only after processing.
func NewConsumer(brokers []string, group string, topics []string, opts ...kgo.Opt) (*kgo.Client, error) {
	base := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.AllowAutoTopicCreation(),
		// A crashed consumer's partitions move to the others after 10s
		// instead of the 45s default.
		kgo.SessionTimeout(10 * time.Second),
		kgo.HeartbeatInterval(2 * time.Second),
	}
	return kgo.NewClient(append(base, opts...)...)
}

// Ping is a readiness check for a Kafka client.
func Ping(cl *kgo.Client) func(context.Context) error {
	return func(ctx context.Context) error { return cl.Ping(ctx) }
}
