// Package metrics exposes the Prometheus registry shared by a service and
// the platform's message-path metrics. Every service serves them on
// OPS_ADDR/metrics; the sign-off dashboards and alerts are built on them.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the service-wide Prometheus registry.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// Handler serves /metrics for Registry.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

var f = promauto.With(Registry)

// Latency buckets from 0.5 ms to 10 s.
var latency = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Message path.
var (
	Accepted = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_accepted_total", Help: "Messages accepted from customers.",
	}, []string{"channel"})
	Refused = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_refused_total", Help: "Customer submits refused at the edge, by reason.",
	}, []string{"channel", "reason"})
	AcceptSeconds = f.NewHistogramVec(prometheus.HistogramOpts{
		Name: "sms_accept_seconds", Help: "Time to accept a message (validate, price, hold, Kafka).", Buckets: latency,
	}, []string{"channel"})
	Routed = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_routed_total", Help: "Messages routed, by vendor (\"none\" = no route).",
	}, []string{"vendor"})
	VendorSubmits = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_vendor_submits_total", Help: "Messages handed to vendors, by outcome (submitted, failed).",
	}, []string{"vendor", "result"})
	VendorSubmitSeconds = f.NewHistogramVec(prometheus.HistogramOpts{
		Name: "sms_vendor_submit_seconds", Help: "Time for a vendor to accept a message (all parts, with retries).", Buckets: latency,
	}, []string{"vendor"})
	VendorDLRs = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_vendor_dlrs_total", Help: "Delivery receipts received from vendors, by stat.",
	}, []string{"vendor", "stat"})
	Final = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_final_total", Help: "Messages reaching a final status (first time).",
	}, []string{"status"})
	CustomerReceipts = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_customer_receipts_total", Help: "Final receipts sent to customers (deliver_sm acknowledged or webhook 2xx).",
	}, []string{"channel"})
	Webhooks = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_webhook_attempts_total", Help: "Webhook delivery attempts, by result (ok, retry, dead).",
	}, []string{"result"})
	Charged = f.NewCounter(prometheus.CounterOpts{
		Name: "sms_charged_messages_total", Help: "Messages charged by the rater.",
	})
	CustomerBinds = f.NewGauge(prometheus.GaugeOpts{
		Name: "sms_customer_binds", Help: "Customer SMPP binds on this node.",
	})
	VendorBindsUp = f.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sms_vendor_binds_up", Help: "Vendor SMPP binds currently bound.",
	}, []string{"vendor"})
	VendorRoutable = f.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sms_vendor_routable", Help: "1 while the vendor is in routing, 0 while failover has taken it out.",
	}, []string{"vendor"})
	FirewallHits = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_firewall_hits_total", Help: "Firewall rule and velocity matches by kind and action (reject or flag).",
	}, []string{"kind", "action"})
	HLRLookups = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_hlr_lookups_total", Help: "HLR answers by source (cache or provider) and status.",
	}, []string{"source", "status"})
	Rerouted = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_rerouted_total", Help: "Messages a connector handed back for another route.",
	}, []string{"from_vendor", "reason"})
)

// Kafka consumers.
var (
	BatchRecords = f.NewHistogramVec(prometheus.HistogramOpts{
		Name: "sms_consumer_batch_records", Help: "Records per processed batch.",
		Buckets: []float64{1, 10, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
	}, []string{"group"})
	BatchSeconds = f.NewHistogramVec(prometheus.HistogramOpts{
		Name: "sms_consumer_batch_seconds", Help: "Time to process a batch, commit excluded.", Buckets: latency,
	}, []string{"group"})
	ConsumedRecords = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_consumed_records_total", Help: "Records processed and committed.",
	}, []string{"group"})
	RecordsDropped = f.NewCounter(prometheus.CounterOpts{
		Name: "sms_records_dropped_total", Help: "Message history rows not written to ClickHouse because they lack valid IDs.",
	})
	BatchRetries = f.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_consumer_batch_retries_total", Help: "Failed batches retried in place (e.g. during a database failover).",
	}, []string{"group"})
)
