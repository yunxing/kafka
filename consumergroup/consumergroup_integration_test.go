package consumergroup

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"testing"
	"time"

	"github.com/Shopify/sarama"
)

const (
	TopicWithSinglePartition    = "consumergroup.single"
	TopicWithMultiplePartitions = "consumergroup.multi"
)

var (
	zookeeper []string = []string{"localhost:2181"}
)

////////////////////////////////////////////////////////////////////
// Examples
////////////////////////////////////////////////////////////////////

func ExampleConsumerGroup() {
	consumer, consumerErr := JoinConsumerGroup(
		"ExampleConsumerGroup",
		[]string{TopicWithSinglePartition, TopicWithMultiplePartitions},
		[]string{"localhost:2181"},
		nil)

	if consumerErr != nil {
		log.Fatalln(consumerErr)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		consumer.Close()
	}()

	eventCount := 0

	for event := range consumer.Messages() {
		// Process event
		log.Println(string(event.Value))
		eventCount += 1

		// Ack event
		consumer.CommitUpto(event)
	}

	log.Printf("Processed %d events.", eventCount)
}

////////////////////////////////////////////////////////////////////
// Integration tests
////////////////////////////////////////////////////////////////////

func TestIntegrationMultipleTopicsSingleConsumer(t *testing.T) {
	consumerGroup := "TestIntegrationMultipleTopicsSingleConsumer"
	setupZookeeper(t, consumerGroup, TopicWithSinglePartition, 1)
	setupZookeeper(t, consumerGroup, TopicWithMultiplePartitions, 4)

	// Produce 100 events that we will consume
	go produceEvents(t, consumerGroup, TopicWithSinglePartition, 100)
	go produceEvents(t, consumerGroup, TopicWithMultiplePartitions, 200)

	consumer, err := JoinConsumerGroup(consumerGroup, []string{TopicWithSinglePartition, TopicWithMultiplePartitions}, zookeeper, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()

	var offsets = make(OffsetMap)
	assertEvents(t, consumer, 300, offsets)
}

func TestIntegrationSingleTopicParallelConsumers(t *testing.T) {
	consumerGroup := "TestIntegrationSingleTopicParallelConsumers"
	setupZookeeper(t, consumerGroup, TopicWithMultiplePartitions, 4)
	go produceEvents(t, consumerGroup, TopicWithMultiplePartitions, 200)

	consumer1, err := JoinConsumerGroup(consumerGroup, []string{TopicWithMultiplePartitions}, zookeeper, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer1.Close()

	consumer2, err := JoinConsumerGroup(consumerGroup, []string{TopicWithMultiplePartitions}, zookeeper, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer2.Close()

	var eventCount1, eventCount2 int64
	offsets := make(map[int32]int64)

	events1 := consumer1.Messages()
	events2 := consumer2.Messages()

	handleEvent := func(message *sarama.ConsumerMessage, ok bool) {
		if !ok {
			t.Fatal("Event stream closed prematurely")
		}

		if offsets[message.Partition] != 0 && offsets[message.Partition]+1 != message.Offset {
			t.Fatalf("Unecpected offset on partition %d. Expected %d, got %d.", message.Partition, offsets[message.Partition]+1, message.Offset)
		}

		offsets[message.Partition] = message.Offset
	}

	for eventCount1+eventCount2 < 200 {
		select {
		case <-time.After(10 * time.Second):
			t.Fatal("Reader timeout")

		case event1, ok1 := <-events1:
			handleEvent(event1, ok1)
			eventCount1 += 1
			consumer1.CommitUpto(event1)

		case event2, ok2 := <-events2:
			handleEvent(event2, ok2)
			eventCount2 += 1
			consumer2.CommitUpto(event2)
		}
	}

	if eventCount1 == 0 || eventCount2 == 0 {
		t.Error("Expected events to be consumed by both consumers!")
	} else {
		t.Logf("Successfully read %d and %d messages, closing!", eventCount1, eventCount2)
	}
}

func TestSingleTopicSequentialConsumer(t *testing.T) {
	consumerGroup := "TestSingleTopicSequentialConsumer"
	setupZookeeper(t, consumerGroup, TopicWithSinglePartition, 1)
	go produceEvents(t, consumerGroup, TopicWithSinglePartition, 20)

	offsets := make(OffsetMap)

	// If the channel is buffered, the consumer will enqueue more events in the channel,
	// which assertEvents will simply skip. When consumer 2 starts it will skip a bunch of
	// events because of this. Transactional processing will fix this.
	config := NewConfig()
	config.ChannelBufferSize = 0

	consumer1, err := JoinConsumerGroup(consumerGroup, []string{TopicWithSinglePartition}, zookeeper, config)
	if err != nil {
		t.Fatal(err)
	}

	assertEvents(t, consumer1, 10, offsets)
	consumer1.Close()

	consumer2, err := JoinConsumerGroup(consumerGroup, []string{TopicWithSinglePartition}, zookeeper, nil)
	if err != nil {
		t.Fatal(err)
	}

	assertEvents(t, consumer2, 10, offsets)
	consumer2.Close()
}

////////////////////////////////////////////////////////////////////
// Helper functions and types
////////////////////////////////////////////////////////////////////

type OffsetMap map[string]map[int32]int64

func assertEvents(t *testing.T, cg *ConsumerGroup, count int64, offsets OffsetMap) {
	var processed int64
	for processed < count {
		select {
		case <-time.After(5 * time.Second):
			t.Fatalf("Reader timeout after %d events!", processed)

		case message, ok := <-cg.Messages():
			if !ok {
				t.Fatal("Event stream closed prematurely")
			}

			if offsets != nil {
				if offsets[message.Topic] == nil {
					offsets[message.Topic] = make(map[int32]int64)
				}
				if offsets[message.Topic][message.Partition] != 0 && offsets[message.Topic][message.Partition]+1 != message.Offset {
					t.Fatalf("Unexpected offset on %s/%d. Expected %d, got %d.", message.Topic, message.Partition, offsets[message.Topic][message.Partition]+1, message.Offset)
				}

				processed += 1
				offsets[message.Topic][message.Partition] = message.Offset
				cg.CommitUpto(message)
			}

		}
	}
	t.Logf("Successfully asserted %d events.", count)
}

func saramaClient() *sarama.Client {
	client, err := sarama.NewClient([]string{"localhost:9092"}, nil)
	if err != nil {
		panic(err)
	}
	return client
}

func produceEvents(t *testing.T, consumerGroup string, topic string, amount int64) error {
	producer, err := sarama.NewSyncProducer([]string{"localhost:9092"}, nil)
	if err != nil {
		return err
	}
	defer producer.Close()

	for i := int64(1); i <= amount; i++ {
		_, _, err = producer.SendMessage(topic, nil, sarama.StringEncoder(fmt.Sprintf("testing %d", i)))

		if err != nil {
			return err
		}
	}

	return nil
}

func setupZookeeper(t *testing.T, consumerGroup string, topic string, partitions int32) {
	client := saramaClient()
	defer client.Close()

	// Connect to zookeeper to commit the last seen offset.
	// This way we should only produce events that we produce ourselves in this test.
	zk, zkErr := NewZK(zookeeper, "", 1*time.Second)
	if zkErr != nil {
		t.Fatal(zkErr)
	}
	defer zk.Close()

	for partition := int32(0); partition < partitions; partition++ {
		// Retrieve the offset that Sarama will use for the next message on the topic/partition.
		nextOffset, offsetErr := client.GetOffset(topic, partition, sarama.LatestOffsets)
		if offsetErr != nil {
			t.Fatal(offsetErr)
		} else {
			t.Logf("Next offset for %s/%d = %d", topic, partition, nextOffset)
		}

		if err := zk.Commit(consumerGroup, topic, partition, nextOffset); err != nil {
			t.Fatal(err)
		}
	}
}
