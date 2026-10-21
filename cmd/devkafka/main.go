// Command devkafka runs an in-memory, Kafka-protocol-compatible broker for
// local development and end-to-end tests where Docker is not available.
// It is not for production: use Kafka or Redpanda there.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/twmb/franz-go/pkg/kfake"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/records"
	"github.com/jpamaze/amazeA2pSMS/internal/webhook"
)

func main() {
	port := flag.Int("port", 9092, "listen port")
	partitions := flag.Int("partitions", 12, "partitions per topic")
	vendors := flag.String("vendors", "sim", "comma-separated vendor IDs to pre-create route topics for")
	dataDir := flag.String("data", "", "directory to persist data (empty = memory only)")
	flag.Parse()

	opts := []kfake.Opt{
		kfake.Ports(*port),
		kfake.AllowAutoTopicCreation(),
		kfake.DefaultNumPartitions(*partitions),
		kfake.SeedTopics(int32(*partitions), append(append(kafka.AllTopics(strings.Split(*vendors, ",")), webhook.Topics()...), records.Topic)...),
	}
	if *dataDir != "" {
		opts = append(opts, kfake.DataDir(*dataDir))
	}
	c, err := kfake.NewCluster(opts...)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	log.Printf("devkafka listening on %v", c.ListenAddrs())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
