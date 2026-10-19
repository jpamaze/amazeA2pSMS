// Package kafka defines topic names, the message envelope and client helpers.
//
// Blueprint rule 1: a customer is acknowledged only after the message is in
// Kafka with acks=all. NewProducer enforces that setting.
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Topic names. Vendor route topics are RouteTopic(vendorID).
const (
	TopicSubmit        = "sms.submit"         // accepted by edge, waiting for routing
	TopicDLR           = "sms.dlr"            // DLRs journaled by connectors before deliver_sm_resp
	TopicMO            = "sms.mo"             // mobile-originated messages from vendors
	TopicBillingEvents = "sms.billing.events" // holds, settles, releases for the rater/ledger
	TopicConfigChanges = "sms.config.changes" // cache invalidation for in-memory config
	routeTopicPrefix   = "sms.route."
)

// RouteTopic is the per-vendor queue a connector consumes.
func RouteTopic(vendorID string) string { return routeTopicPrefix + vendorID }

// Envelope wraps every event on the bus.
type Envelope struct {
	// IdempotencyKey is account ID + customer reference for submits and
	// vendor ID + vendor message ID for DLRs (blueprint rule 5).
	IdempotencyKey string          `json:"idempotency_key"`
	Type           string          `json:"type"`
	AccountID      string          `json:"account_id,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	Payload        json.RawMessage `json:"payload"`
}

// SubmitKey builds the idempotency key for a customer submit.
func SubmitKey(accountID, clientRef string) string {
	return fmt.Sprintf("submit:%s:%s", accountID, clientRef)
}

// DLRKey builds the idempotency key for a vendor DLR.
func DLRKey(vendorID, vendorMsgID string) string {
	return fmt.Sprintf("dlr:%s:%s", vendorID, vendorMsgID)
}

// NewProducer returns a client tuned for durable, idempotent produce.
func NewProducer(brokers []string, opts ...kgo.Opt) (*kgo.Client, error) {
	base := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.ZstdCompression()),
		kgo.ProducerLinger(2 * time.Millisecond),
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
	}
	return kgo.NewClient(append(base, opts...)...)
}

// Ping is a readiness check for a Kafka client.
func Ping(cl *kgo.Client) func(context.Context) error {
	return func(ctx context.Context) error { return cl.Ping(ctx) }
}
