package kafka

// Handler will handle the schedules triggered by the scheduler,
// in this case it will send the message to the target topic, publish a
// tombstone message (to delete the schedule in the scheduler topic)
// and log the triggered message in a history topic
import (
	"fmt"
	"strconv"

	log "github.com/sirupsen/logrus"

	confluent "github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/etf1/kafka-message-scheduler/schedule"
	"github.com/etf1/kafka-message-scheduler/schedule/kafka"
	"github.com/etf1/kafka-message-scheduler/scheduler"
)

const (
	// OriginalTimestamp will store the timestamp of the message in the scheduler topic
	OriginalTimestamp = "scheduler-timestamp"
	// OriginalKey stores the original schedule key
	OriginalKey = "scheduler-key"
	// OriginalKey stores the scheduler topic where it came from
	OriginalTopic = "scheduler-topic"

	flushTimeoutMs = 10000
)

type EventHandler struct {
	historyTopic string
	producer     *confluent.Producer
}

func NewHandler(bootstrapServers, historyTopic string) (EventHandler, error) {
	if bootstrapServers == "" {
		return EventHandler{}, fmt.Errorf("bootstrapServers input cannot be empty")
	}

	if historyTopic == "" {
		return EventHandler{}, fmt.Errorf("historyTopic input cannot be empty")
	}

	producer, err := confluent.NewProducer(&confluent.ConfigMap{
		"bootstrap.servers": bootstrapServers,
	})
	if err != nil {
		return EventHandler{}, err
	}

	k := EventHandler{
		historyTopic: historyTopic,
		producer:     producer,
	}

	topic := func(msg *confluent.Message) string {
		return *msg.TopicPartition.Topic
	}
	emptyValue := func(msg *confluent.Message) bool {
		return len(msg.Value) == 0
	}
	key := func(msg *confluent.Message) string {
		return string(msg.Key)
	}

	// kafa producer delivery report go routine
	go func() {
		defer log.Println("kafka producer stopped")
		for e := range producer.Events() {
			switch ev := e.(type) {
			case *confluent.Message:
				if ev.TopicPartition.Error != nil {
					log.Errorf("delivery failed: %v", ev.TopicPartition.Error)
					break
				}
				// if not message from history and not a tombstone message, then it is a regular schedule message
				if topic(ev) != historyTopic && !emptyValue(ev) {
					err := k.produceTombstoneMessage(ev)
					if err != nil {
						log.Errorf("unable to produce tombstone message with id %q: %v\n", key(ev), err)
					}
					err = k.produceHistoryMessage(ev)
					if err != nil {
						log.Errorf("unable to produce history message with id %q: %v\n", key(ev), err)
					}
				}
			case confluent.Error:
				log.Errorf("received an error with code %v: %v\n", ev.Code(), ev)
			default:
				log.Errorf("ignoring event: %s\n", ev)
			}
		}
	}()

	return k, nil
}

func (k EventHandler) Close() {
	defer log.Println("kafka handler closed")
	defer k.producer.Close()
	defer k.producer.Flush(flushTimeoutMs)

	log.Println("kafka handler closing ...")
}

func (k EventHandler) String() string {
	return fmt.Sprintf("kafka handler history_topic=%v\n", k.historyTopic)
}

// store in a specific topic the triggered messages
func (k EventHandler) produceHistoryMessage(msg *confluent.Message) error {
	headers := getHeadersFromOpaque(msg)
	historyMsg := confluent.Message{
		TopicPartition: confluent.TopicPartition{Topic: &k.historyTopic, Partition: confluent.PartitionAny},
		Key:            msg.Key,
		Value:          msg.Value,
		Headers:        headers,
	}

	log.Debugf("producing history message with id %q on topic %q\n", string(msg.Key), k.historyTopic)

	return k.producer.Produce(&historyMsg, nil)
}

func getHeadersFromOpaque(msg *confluent.Message) []confluent.Header {
	opaque, ok := msg.Opaque.(HandlerOpaque)
	if !ok {
		return nil
	}
	return opaque.headers
}

func getHeaderValue(headers []confluent.Header, key string) (string, bool) {
	for _, header := range headers {
		if header.Key == key && len(header.Value) > 0 {
			return string(header.Value), true
		}
	}
	return "", false
}

func (k EventHandler) produceTombstoneMessage(msg *confluent.Message) error {
	headers := getHeadersFromOpaque(msg)

	originalKey, foundKey := getHeaderValue(headers, OriginalKey)
	originalTopic, foundTopic := getHeaderValue(headers, OriginalTopic)

	if !foundKey {
		return fmt.Errorf("cannot find original key in the headers")
	}

	if !foundTopic {
		return fmt.Errorf("cannot find original topic in the headers")
	}

	tombstoneMsg := confluent.Message{
		TopicPartition: confluent.TopicPartition{Topic: &originalTopic, Partition: confluent.PartitionAny},
		Key:            []byte(originalKey),
		// tombstone is message with nil or empty value
		Value:   nil,
		Headers: headers,
	}

	log.Debugf("producing tombstone message with id %q on topic %q\n", originalKey, originalTopic)

	return k.producer.Produce(&tombstoneMsg, nil)
}

// in the confluent go lib, in the delivery channel, the original timestamp and headers
// are not available, so we need to passt them hrough via the Opaque field
type HandlerOpaque struct {
	headers []confluent.Header
}

func (k EventHandler) produceTargetMessage(msg kafka.Schedule) error {
	headers := append(
		msg.Headers,
		confluent.Header{
			Key:   OriginalTimestamp,
			Value: []byte(strconv.FormatInt(msg.Timestamp(), 10)),
		},
		confluent.Header{
			Key:   OriginalKey,
			Value: msg.Key,
		},
		confluent.Header{
			Key:   OriginalTopic,
			Value: []byte(*msg.TopicPartition.Topic),
		},
	)

	targetTopic := msg.TargetTopic()

	targetMsg := confluent.Message{
		TopicPartition: confluent.TopicPartition{Topic: &targetTopic, Partition: confluent.PartitionAny},
		Key:            []byte(msg.TargetKey()),
		Value:          msg.Value,
		Headers:        headers,
	}

	// We are setting the headers in the Opaque field because we want them
	// to be available in the producer.Events() channel.
	// Today Timestamps and Headers are not available in the producer.Events() delivery report channel
	targetMsg.Opaque = HandlerOpaque{
		headers: targetMsg.Headers,
	}

	log.Debugf("producing target message with id %q on topic %q\n", msg.TargetKey(), targetTopic)

	return k.producer.Produce(&targetMsg, nil)
}

func (k EventHandler) Handle(event scheduler.Event) {
	switch evt := event.(type) {
	case schedule.InvalidSchedule:
		log.Debugf("received an InvalidSchedule event: %T %+v errors=%v\n", evt, evt, evt.Errors)
	case schedule.MissedSchedule:
		log.Debugf("received a MissedSchedule event: %T %v\n", evt, evt)
		msg, ok := evt.Schedule.(kafka.Schedule)
		if !ok {
			log.Errorf("event is not a kafka.Schedule: %T %+v\n", event, event)
			break
		}
		err := k.produceTargetMessage(msg)
		if err != nil {
			log.Errorf("unable to produce the message: %v %v\n", err, msg)
		}
	case schedule.Schedule:
		log.Printf("received a regular schedule event: %T %v\n", evt, evt)
		msg, ok := evt.(kafka.Schedule)
		if !ok {
			log.Errorf("event is not a kafka.Schedule: %T %+v\n", event, event)
			break
		}
		err := k.produceTargetMessage(msg)
		if err != nil {
			log.Errorf("unable to produce the message: %v %v\n", err, msg)
		}
	default:
		log.Errorf("unexpected event type: %T %v\n", evt, evt)
	}
}
