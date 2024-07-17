package main

import (
	"errors"
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
)

type MockChannel struct {
	exchangeName string
	queues       map[string][]amqp.Publishing
	mappings     map[string]string // maps routing keys to queue names
	ack bool
	nack bool
}

func CreateMockChannel(config map[string]any) *MockChannel {
	ch := new(MockChannel)
	ch.queues = make(map[string][]amqp.Publishing)
	ch.mappings = make(map[string]string)
	exchanges := config["exchanges"].([]any)
	for _, exchange_list := range exchanges {
		exchange := exchange_list.(map[string]any)
		ch.exchangeName = exchange["name"].(string)
	}
	bindings := config["bindings"].([]any)
	for _, binding_list := range bindings {
		binding := binding_list.(map[string]any)
		routingKey := binding["routing_key"].(string)
		queueName := binding["destination"].(string)
		ch.mappings[routingKey] = queueName
	}
	return ch
}

func (ch *MockChannel) Ack(tag uint64, multiple bool) error {
	ch.ack = true
	return nil
}
func (ch *MockChannel) Nack(tag uint64, multiple bool, requeue bool) error {
	ch.nack = true
	return nil
}
func (ch *MockChannel) Reject(tag uint64, requeue bool) error {
	return nil
}

func (ch *MockChannel) Publish(exchange, key string, mandatory bool, immediate bool, msg amqp.Publishing) error {
	queuename, ok := ch.mappings[key]
	if !ok {
		queuename = key
		// fmt.Printf("Mapped routing key [%s] to default queue value [%s]\n", key, queuename)
	} else {
		// fmt.Printf("Mapped routing key [%s] to queue [%s]\n", key, queuename)
	}
	if exchange == ch.exchangeName {
		ch.queues[queuename] = append(ch.queues[queuename], msg)
	} else {
		errormessage := fmt.Sprintf("MockChannel.Publish Message not posted to correct exchange: expected='%s' but got='%s'\n", ch.exchangeName, exchange)
		return errors.New(errormessage)
	}
	return nil
}

func (ch *MockChannel) GetMessage(queue string) *amqp.Publishing {
	if len(ch.queues[queue]) > 0 {
		msg := ch.queues[queue][0]
		ch.queues[queue] = ch.queues[queue][1:]
		return &msg
	}
	return nil
}

// return the current Ack and Nack values for the channel and reset them afterwards
func (ch *MockChannel) GetAckNack() (bool,bool) {
        ack  := ch.ack
        nack := ch.nack
        ch.ack = false
        ch.nack = false
        return ack, nack
}