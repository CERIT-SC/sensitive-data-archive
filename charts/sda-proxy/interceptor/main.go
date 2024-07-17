// Package main contains the main logic of the "mq-interceptor" microservice.
package main

import (
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "github.com/lib/pq"
	amqp "github.com/rabbitmq/amqp091-go"
	"log"
	"net/url"
	"os"
	"sync"
	"time"
)

// This interface serves as the supertype for both amqp.Channel and the MockChannel used for testing
type MQChannel interface {
        Ack(tag uint64, multiple bool) error
        Nack(tag uint64, multiple bool, requeue bool) error
        Reject(tag uint64, requeue bool) error
        Publish(exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
}

var db *sql.DB
var publishMutex sync.Mutex


// dialRabbitMQ attempts to connect to RabbitMQ up to 10 times
// with a delay between retries. It returns a connection
// instance or an error.
func dialRabbitMQ(connectionString string) (*amqp.Connection, error) {
	var conn *amqp.Connection
	var err error
	var attempts = 10
	// Parse the connection string as a URL.
	u, err := url.Parse(connectionString)
	if err != nil {
		return nil, err
	}
	log.Printf("Trying to dial host: %s [I will attempt to dial %d times with 2 seconds interval]", u.Hostname(), attempts)
	log.Printf("Is TLS enabled? %t", os.Getenv("ENABLE_TLS") == "true")
	for i := 0; i < attempts; i++ {
		if os.Getenv("ENABLE_TLS") == "true" {
			conn, err = amqp.DialTLS(connectionString, getTLSConfig())
		} else {
			conn, err = amqp.Dial(connectionString)
		}
		if err == nil {
			log.Printf("Successfully connected to host %s\n", u.Hostname())
			return conn, nil
		}
		log.Printf("Attempt %d: Failed to connect to RabbitMQ: %s\n", i+1, err)
		time.Sleep(2 * time.Second) // Wait before retrying
	}
	// After all attempts, return the last error
	return nil, err
}

func main() {

	var err error

	db, err = sql.Open("postgres", os.Getenv("POSTGRES_CONNECTION"))
	failOnError(err, "Failed to connect to DB")

	legaMqConnString := os.Getenv("LEGA_MQ_CONNECTION")
	legaMQ, err := dialRabbitMQ(legaMqConnString)
    failOnError(err, "Failed to connect to LEGA queue after many attempts")
	legaConsumeChannel, err := legaMQ.Channel()
	failOnError(err, "Failed to create LEGA consume RabbitMQ channel")
	legaPublishChannel, err := legaMQ.Channel()
	failOnError(err, "Failed to create LEGA publish RabbitMQ channel")
	legaNotifyCloseChannel := legaMQ.NotifyClose(make(chan *amqp.Error))
	go func() {
		err := <-legaNotifyCloseChannel
		log.Fatal(err)
	}()

	cegaMqConnString := os.Getenv("CEGA_MQ_CONNECTION")
	cegaMQ, err := dialRabbitMQ(cegaMqConnString)
    failOnError(err, "Failed to connect to CEGA queue after many attempts")
	cegaConsumeChannel, err := cegaMQ.Channel()
	failOnError(err, "Failed to create CEGA consume RabbitMQ channel")
	cegaPublishChannel, err := cegaMQ.Channel()
	failOnError(err, "Failed to create CEGA publish RabbitMQ channel")
	cegaNotifyCloseChannel := cegaMQ.NotifyClose(make(chan *amqp.Error))
	go func() {
		err := <-cegaNotifyCloseChannel
		log.Fatal(err)
	}()
	errorPublishChannel := cegaPublishChannel

	cegaQueue := os.Getenv("CEGA_MQ_QUEUE")
	cegaExchange := os.Getenv("CEGA_MQ_EXCHANGE")
	legaExchange := os.Getenv("LEGA_MQ_EXCHANGE")

	cegaDeliveries, err := cegaConsumeChannel.Consume(cegaQueue, "", false, false, false, false, nil)
	failOnError(err, "Failed to connect to CEGA queue: "+cegaQueue)
	go func() {
		for delivery := range cegaDeliveries {
			forwardDeliveryTo(true, cegaConsumeChannel, legaPublishChannel, errorPublishChannel, legaExchange, "", delivery)
		}
	}()

	errorDeliveries, err := legaConsumeChannel.Consume("error", "", false, false, false, false, nil)
	failOnError(err, "Failed to connect to 'error' queue")
	go func() {
		for delivery := range errorDeliveries {
			forwardDeliveryTo(false, legaConsumeChannel, cegaPublishChannel, errorPublishChannel, cegaExchange, "files.error", delivery)
		}
	}()

	verifiedDeliveries, err := legaConsumeChannel.Consume("verified", "", false, false, false, false, nil)
	failOnError(err, "Failed to connect to 'verified' queue")
	go func() {
		for delivery := range verifiedDeliveries {
			forwardDeliveryTo(false, legaConsumeChannel, cegaPublishChannel, errorPublishChannel, cegaExchange, "files.verified", delivery)
		}
	}()

	completedDeliveries, err := legaConsumeChannel.Consume("completed", "", false, false, false, false, nil)
	failOnError(err, "Failed to connect to 'completed' queue")
	go func() {
		for delivery := range completedDeliveries {
			forwardDeliveryTo(false, legaConsumeChannel, cegaPublishChannel, errorPublishChannel, cegaExchange, "files.completed", delivery)
		}
	}()

	inboxDeliveries, err := legaConsumeChannel.Consume("inbox", "", false, false, false, false, nil)
	failOnError(err, "Failed to connect to 'inbox' queue")
	go func() {
		for delivery := range inboxDeliveries {
			forwardDeliveryTo(false, legaConsumeChannel, cegaPublishChannel, errorPublishChannel, cegaExchange, "files.inbox", delivery)
		}
	}()

	forever := make(chan bool)
	log.Printf(" [*] Waiting for messages. To exit press CTRL+C")
	<-forever
}

func forwardDeliveryTo(fromCEGAToLEGA bool, channelFrom MQChannel, channelTo MQChannel, errorChannel MQChannel, exchange string, routingKey string, delivery amqp.Delivery) {
	publishMutex.Lock()
	defer publishMutex.Unlock()
	publishing, messageType, err := buildPublishingFromDelivery(fromCEGAToLEGA, delivery)
	if err != nil {
		log.Printf("%s", err)
		nackError := channelFrom.Nack(delivery.DeliveryTag, false, false)
		failOnError(nackError, "Failed to Nack message")
		err = publishError(delivery, err, errorChannel)
		failOnError(err, "Failed to publish error message")
		return
	}
	// Forward all messages from CEGA to a local queue handled by the SDA intercept service
	if fromCEGAToLEGA {
		routingKey = os.Getenv("LEGA_MQ_QUEUE")
	} else if messageType != nil {
		routingKey = messageType.(string)
	}
	err = channelTo.Publish(exchange, routingKey, false, false, *publishing)
	if err != nil {
		log.Printf("%s", err)
		err := channelFrom.Nack(delivery.DeliveryTag, false, true)
		failOnError(err, "Failed to Nack message")
	} else {
		err = channelFrom.Ack(delivery.DeliveryTag, false)
		failOnError(err, "Failed to Ack message")
		log.Printf("Forwarded message from [%s, %s] to [%s, %s]", delivery.Exchange, delivery.RoutingKey, exchange, routingKey)
		log.Printf("Correlation ID: %s", delivery.CorrelationId)
		log.Printf("Message: %s", string(delivery.Body))
	}
}

func buildPublishingFromDelivery(fromCEGAToLEGA bool, delivery amqp.Delivery) (*amqp.Publishing, interface{}, error) {
	publishing := amqp.Publishing{
		Headers:         delivery.Headers,
		ContentType:     delivery.ContentType,
		ContentEncoding: delivery.ContentEncoding,
		DeliveryMode:    delivery.DeliveryMode,
		Priority:        delivery.Priority,
		CorrelationId:   delivery.CorrelationId,
		ReplyTo:         delivery.ReplyTo,
		Expiration:      delivery.Expiration,
		MessageId:       delivery.MessageId,
		Timestamp:       delivery.Timestamp,
		Type:            delivery.Type,
		UserId:          delivery.UserId,
		AppId:           delivery.AppId,
	}

	message := make(map[string]interface{}, 0)
	err := json.Unmarshal(delivery.Body, &message)
	if err != nil {
		return nil, nil, err
	}

	messageType, _ := message["type"]

	user, ok := message["user"]
	if !ok {
		publishing.Body = delivery.Body
		return &publishing, messageType, nil
	}

	stringUser := fmt.Sprintf("%s", user)

	if fromCEGAToLEGA {
		elixirId, err := selectElixirIdByEGAId(stringUser)
		if err != nil {
			return nil, "", err
		}
		message["user"] = elixirId
	} else {
		egaId, err := selectEgaIdByElixirId(stringUser)
		if err != nil {
			return nil, "", err
		}
		message["user"] = egaId
	}

	publishing.Body, err = json.Marshal(message)

	return &publishing, messageType, err
}

func publishError(delivery amqp.Delivery, err error, errorChannel MQChannel) error {
	errorMessage := fmt.Sprintf("{\"reason\" : \"%s\", \"original_message\" : \"%s\"}", err.Error(), string(delivery.Body))
	publishing := amqp.Publishing{
		ContentType:     delivery.ContentType,
		ContentEncoding: delivery.ContentEncoding,
		CorrelationId:   delivery.CorrelationId,
		Body:            []byte(errorMessage),
	}
	err = errorChannel.Publish(os.Getenv("CEGA_MQ_EXCHANGE"), "files.error", false, false, publishing)
	return err
}

func selectElixirIdByEGAId(egaId string) (elixirId string, err error) {
	err = db.QueryRow("select elixir_id from mapping where ega_id = $1", egaId).Scan(&elixirId)
	if err == nil {
		log.Printf("Replacing EGA ID [%s] with Elixir ID [%s]", egaId, elixirId)
	}
	return
}

func selectEgaIdByElixirId(elixirId string) (egaId string, err error) {
	err = db.QueryRow("select ega_id from mapping where elixir_id = $1", elixirId).Scan(&egaId)
	if err == nil {
		log.Printf("Replacing Elixir ID [%s] with EGA ID [%s]", elixirId, egaId)
	}
	return
}

func getTLSConfig() *tls.Config {
	tlsConfig := tls.Config{}
	if os.Getenv("VERIFY_CERT") == "true" {
		tlsConfig.InsecureSkipVerify = false

		certFile := os.Getenv("INTERCEPTOR_CLIENT_CERT")
		keyFile := os.Getenv("INTERCEPTOR_CLIENT_KEY")

		if certFile == "" || keyFile == "" {
			log.Fatalf("INTERCEPTOR_CLIENT_CERT and INTERCEPTOR_CLIENT_KEY environment variables must be set")
		}

		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			log.Fatalf("Failed to load TLS certificate and key: %v", err)
		}

		tlsConfig.Certificates = []tls.Certificate{cert}

	} else {
		tlsConfig.InsecureSkipVerify = true
	}
	return &tlsConfig
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}
