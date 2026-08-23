// Package anomaly provides opt-in publishing of anomaly events to RabbitMQ.
// When RABBITMQ_URL is not set all events are logged locally and no AMQP
// connection is attempted — the service continues to work without a broker.
package anomaly

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ochestra-tech/k8s-monitor/pkg/anomaly"
)

const (
	defaultExchange   = "k8s.anomalies"
	defaultRoutingKey = "anomaly"
)

// Publisher sends anomaly events to a RabbitMQ topic exchange.
// All fields are safe to use after construction via NewPublisher.
type Publisher struct {
	conn     *amqp.Connection
	ch       *amqp.Channel
	exchange string
}

// NewPublisher dials RABBITMQ_URL and declares the exchange.
// Returns nil (not an error) when RABBITMQ_URL is unset — callers must check for nil
// before calling Publish.
func NewPublisher() *Publisher {
	url := os.Getenv("RABBITMQ_URL")
	if url == "" {
		return nil
	}
	exchange := os.Getenv("ANOMALY_RABBITMQ_EXCHANGE")
	if exchange == "" {
		exchange = defaultExchange
	}

	conn, err := amqp.DialConfig(url, amqp.Config{
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
	})
	if err != nil {
		log.Printf("[anomaly publisher] WARN: could not connect to RabbitMQ (%v) — anomaly events will be logged only", err)
		return nil
	}

	ch, err := conn.Channel()
	if err != nil {
		log.Printf("[anomaly publisher] WARN: could not open channel (%v) — anomaly events will be logged only", err)
		_ = conn.Close()
		return nil
	}

	if err := ch.ExchangeDeclare(
		exchange,
		"topic",
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		log.Printf("[anomaly publisher] WARN: could not declare exchange (%v) — anomaly events will be logged only", err)
		_ = ch.Close()
		_ = conn.Close()
		return nil
	}

	log.Printf("[anomaly publisher] connected to RabbitMQ, exchange=%s", exchange)
	return &Publisher{conn: conn, ch: ch, exchange: exchange}
}

// Publish sends a slice of AnomalyEvents.  Each event is routed as
// "anomaly.{severity}" so consumers can subscribe to severity subsets.
// If p is nil (no broker configured) the events are only logged.
func (p *Publisher) Publish(ctx context.Context, events []anomaly.AnomalyEvent) {
	for _, ev := range events {
		body, err := json.Marshal(ev)
		if err != nil {
			log.Printf("[anomaly publisher] marshal error: %v", err)
			continue
		}

		routingKey := defaultRoutingKey + "." + string(ev.Severity)

		if p == nil || p.ch == nil {
			log.Printf("[anomaly] %s cluster=%s metric=%s value=%.2f z=%.2f",
				ev.Severity, ev.ClusterID, ev.Metric, ev.Value, ev.ZScore)
			continue
		}

		if err := p.ch.PublishWithContext(ctx,
			p.exchange,
			routingKey,
			false, // mandatory
			false, // immediate
			amqp.Publishing{
				ContentType:  "application/json",
				DeliveryMode: amqp.Persistent,
				Timestamp:    ev.DetectedAt,
				Body:         body,
			},
		); err != nil {
			log.Printf("[anomaly publisher] publish error (routing_key=%s): %v", routingKey, err)
		}
	}
}

// Close releases the AMQP channel and connection.  Safe to call on nil.
func (p *Publisher) Close() {
	if p == nil {
		return
	}
	if p.ch != nil {
		_ = p.ch.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
}
