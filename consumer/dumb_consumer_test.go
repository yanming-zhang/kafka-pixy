package consumer

import (
	"io"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/mailgun/kafka-pixy/testhelpers"
	"github.com/mailgun/log"
	. "gopkg.in/check.v1"
)

type PartitionConsumerSuite struct{}

var (
	_       = Suite(&PartitionConsumerSuite{})
	testMsg = sarama.StringEncoder("Foo")
)

func (s *PartitionConsumerSuite) SetUpSuite(c *C) {
	testhelpers.InitLogging(c)
}

// If a particular offset is provided then messages are consumed starting from
// that offset.
func (s *PartitionConsumerSuite) TestOffsetManual(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)

	mockFetchResponse := sarama.NewMockFetchResponse(c, 1)
	for i := 0; i < 10; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(i+1234), testMsg)
	}

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 2345),
		"FetchRequest": mockFetchResponse,
	})

	// When
	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	consumer, concreteOffset, err := master.ConsumePartition("my_topic", 0, 1234)
	c.Assert(err, IsNil)
	c.Assert(concreteOffset, Equals, int64(1234))

	// Then: messages starting from offset 1234 are consumed.
	for i := 0; i < 10; i++ {
		select {
		case message := <-consumer.Messages():
			c.Assert(message.Offset, Equals, int64(i+1234))
		case err := <-consumer.Errors():
			c.Error(err)
		}
	}

	safeClose(c, consumer)
	safeClose(c, master)
	broker0.Close()
}

// If `sarama.OffsetNewest` is passed as the initial offset then the first consumed
// message is indeed corresponds to the offset that broker claims to be the
// newest in its metadata response.
func (s *PartitionConsumerSuite) TestOffsetNewest(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 10).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 7),
		"FetchRequest": sarama.NewMockFetchResponse(c, 1).
			SetMessage("my_topic", 0, 9, testMsg).
			SetMessage("my_topic", 0, 10, testMsg).
			SetMessage("my_topic", 0, 11, testMsg).
			SetHighWaterMark("my_topic", 0, 14),
	})

	f, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	// When
	pc, concreteOffset, err := f.ConsumePartition("my_topic", 0, sarama.OffsetNewest)
	c.Assert(err, IsNil)
	c.Assert(concreteOffset, Equals, int64(10))

	// Then
	msg := <-pc.Messages()
	c.Assert(msg.Offset, Equals, int64(10))
	c.Assert(msg.HighWaterMark, Equals, int64(14))

	safeClose(c, pc)
	safeClose(c, f)
	broker0.Close()
}

// It is possible to close a partition consumer and create the same anew.
func (s *PartitionConsumerSuite) TestRecreate(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(c, 1).
			SetMessage("my_topic", 0, 10, testMsg),
	})

	f, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	pc, _, err := f.ConsumePartition("my_topic", 0, 10)
	c.Assert(err, IsNil)
	c.Assert((<-pc.Messages()).Offset, Equals, int64(10))

	// When
	safeClose(c, pc)
	pc, _, err = f.ConsumePartition("my_topic", 0, 10)
	c.Assert(err, IsNil)

	// Then
	c.Assert((<-pc.Messages()).Offset, Equals, int64(10))

	safeClose(c, pc)
	safeClose(c, f)
	broker0.Close()
}

// An attempt to consume the same partition twice should fail.
func (s *PartitionConsumerSuite) TestDuplicate(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(c, 1),
	})

	config := sarama.NewConfig()
	config.ChannelBufferSize = 0
	f, err := NewConsumer([]string{broker0.Addr()}, config)
	c.Assert(err, IsNil)

	pc1, _, err := f.ConsumePartition("my_topic", 0, 0)
	c.Assert(err, IsNil)

	// When
	pc2, _, err := f.ConsumePartition("my_topic", 0, 0)

	// Then
	if pc2 != nil || err != sarama.ConfigurationError("That topic/partition is already being consumed") {
		c.Error("A partition cannot be consumed twice at the same time")
	}

	safeClose(c, pc1)
	safeClose(c, f)
	broker0.Close()
}

// If consumer fails to refresh metadata it keeps retrying with frequency
// specified by `Config.Consumer.Retry.Backoff`.
func (s *PartitionConsumerSuite) TestLeaderRefreshError(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 100)

	// Stage 1: my_topic/0 served by broker0
	log.Infof("    STAGE 1")

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 123).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(c, 1).
			SetMessage("my_topic", 0, 123, testMsg),
	})

	config := sarama.NewConfig()
	config.Net.ReadTimeout = 100 * time.Millisecond
	config.Consumer.Retry.Backoff = 200 * time.Millisecond
	config.Consumer.Return.Errors = true
	config.Metadata.Retry.Max = 0
	f, err := NewConsumer([]string{broker0.Addr()}, config)
	c.Assert(err, IsNil)

	pc, _, err := f.ConsumePartition("my_topic", 0, sarama.OffsetOldest)
	c.Assert(err, IsNil)
	c.Assert((<-pc.Messages()).Offset, Equals, int64(123))

	// Stage 2: broker0 says that it is no longer the leader for my_topic/0,
	// but the requests to retrieve metadata fail with network timeout.
	log.Infof("    STAGE 2")

	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNotLeaderForPartition)

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse2),
	})

	if consErr := <-pc.Errors(); consErr.Err != sarama.ErrNotLeaderForPartition {
		c.Errorf("Unexpected error: %v", consErr.Err)
	}

	// Stage 3: finally the metadata returned by broker0 tells that broker1 is
	// a new leader for my_topic/0. Consumption resumes.

	log.Infof("    STAGE 3")

	broker1 := sarama.NewMockBroker(c, 101)

	broker1.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockFetchResponse(c, 1).
			SetMessage("my_topic", 0, 124, testMsg),
	})
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetBroker(broker1.Addr(), broker1.BrokerID()).
			SetLeader("my_topic", 0, broker1.BrokerID()),
	})

	c.Assert((<-pc.Messages()).Offset, Equals, int64(124))

	safeClose(c, pc)
	safeClose(c, f)
	broker1.Close()
	broker0.Close()
}

func (s *PartitionConsumerSuite) TestInvalidTopic(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 100)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()),
	})

	f, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	// When
	pc, _, err := f.ConsumePartition("my_topic", 0, sarama.OffsetOldest)

	// Then
	if pc != nil || err != sarama.ErrUnknownTopicOrPartition {
		c.Errorf("Should fail with, err=%v", err)
	}

	safeClose(c, f)
	broker0.Close()
}

// Nothing bad happens if a partition consumer that has no leader assigned at
// the moment is closed.
func (s *PartitionConsumerSuite) TestClosePartitionWithoutLeader(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 100)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 123).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1000),
		"FetchRequest": sarama.NewMockFetchResponse(c, 1).
			SetMessage("my_topic", 0, 123, testMsg),
	})

	config := sarama.NewConfig()
	config.Net.ReadTimeout = 100 * time.Millisecond
	config.Consumer.Retry.Backoff = 100 * time.Millisecond
	config.Consumer.Return.Errors = true
	config.Metadata.Retry.Max = 0
	f, err := NewConsumer([]string{broker0.Addr()}, config)
	c.Assert(err, IsNil)

	pc, _, err := f.ConsumePartition("my_topic", 0, sarama.OffsetOldest)
	c.Assert(err, IsNil)
	c.Assert((<-pc.Messages()).Offset, Equals, int64(123))

	// broker0 says that it is no longer the leader for my_topic/0, but the
	// requests to retrieve metadata fail with network timeout.
	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNotLeaderForPartition)

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse2),
	})

	// When
	if consErr := <-pc.Errors(); consErr.Err != sarama.ErrNotLeaderForPartition {
		c.Errorf("Unexpected error: %v", consErr.Err)
	}

	// Then: the partition consumer can be closed without any problem.
	safeClose(c, pc)
	safeClose(c, f)
	broker0.Close()
}

// If the initial offset passed on partition consumer creation is out of the
// actual offset range for the partition, then the partition consumer stops
// immediately closing its output channels.
func (s *PartitionConsumerSuite) TestShutsDownOutOfRange(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	fetchResponse := new(sarama.FetchResponse)
	fetchResponse.AddError("my_topic", 0, sarama.ErrOffsetOutOfRange)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1234).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 7),
		"FetchRequest": sarama.NewMockWrapper(fetchResponse),
	})

	f, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	// When
	pc, _, err := f.ConsumePartition("my_topic", 0, 101)
	c.Assert(err, IsNil)

	// Then: consumer should shut down closing its messages and errors channels.
	if _, ok := <-pc.Messages(); ok {
		c.Error("Expected the consumer to shut down")
	}
	safeClose(c, pc)
	safeClose(c, f)
	broker0.Close()
}

// If a fetch response contains messages with offsets that are smaller then
// requested, then such messages are ignored.
func (s *PartitionConsumerSuite) TestExtraOffsets(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	fetchResponse1 := &sarama.FetchResponse{}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 1)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 2)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 3)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 4)
	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNoError)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1234).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0),
		"FetchRequest": sarama.NewMockSequence(fetchResponse1, fetchResponse2),
	})

	f, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	// When
	pc, _, err := f.ConsumePartition("my_topic", 0, 3)
	c.Assert(err, IsNil)

	// Then: messages with offsets 1 and 2 are not returned even though they
	// are present in the response.
	c.Assert((<-pc.Messages()).Offset, Equals, int64(3))
	c.Assert((<-pc.Messages()).Offset, Equals, int64(4))

	safeClose(c, pc)
	safeClose(c, f)
	broker0.Close()
}

// It is fine if offsets of fetched messages are not sequential (although
// strictly increasing!).
func (s *PartitionConsumerSuite) TestNonSequentialOffsets(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	fetchResponse1 := &sarama.FetchResponse{}
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 5)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 7)
	fetchResponse1.AddMessage("my_topic", 0, nil, testMsg, 11)
	fetchResponse2 := &sarama.FetchResponse{}
	fetchResponse2.AddError("my_topic", 0, sarama.ErrNoError)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1234).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 0),
		"FetchRequest": sarama.NewMockSequence(fetchResponse1, fetchResponse2),
	})

	master, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	// When
	pc, _, err := master.ConsumePartition("my_topic", 0, 3)
	c.Assert(err, IsNil)

	// Then: messages with offsets 1 and 2 are not returned even though they
	// are present in the response.
	c.Assert((<-pc.Messages()).Offset, Equals, int64(5))
	c.Assert((<-pc.Messages()).Offset, Equals, int64(7))
	c.Assert((<-pc.Messages()).Offset, Equals, int64(11))

	safeClose(c, pc)
	safeClose(c, master)
	broker0.Close()
}

// If leadership for a partition is changing then consumer resolves the new
// leader and switches to it.
func (s *PartitionConsumerSuite) TestRebalancingMultiplePartitions(c *C) {
	// initial setup
	seedBroker := sarama.NewMockBroker(c, 10)
	leader0 := sarama.NewMockBroker(c, 0)
	leader1 := sarama.NewMockBroker(c, 1)

	seedBroker.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(leader0.Addr(), leader0.BrokerID()).
			SetBroker(leader1.Addr(), leader1.BrokerID()).
			SetLeader("my_topic", 0, leader0.BrokerID()).
			SetLeader("my_topic", 1, leader1.BrokerID()),
	})

	mockOffsetResponse1 := sarama.NewMockOffsetResponse(c).
		SetOffset("my_topic", 0, sarama.OffsetOldest, 0).
		SetOffset("my_topic", 0, sarama.OffsetNewest, 1000).
		SetOffset("my_topic", 1, sarama.OffsetOldest, 0).
		SetOffset("my_topic", 1, sarama.OffsetNewest, 1000)
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"OffsetRequest": mockOffsetResponse1,
		"FetchRequest":  sarama.NewMockFetchResponse(c, 1),
	})
	leader1.SetHandlerByMap(map[string]sarama.MockResponse{
		"OffsetRequest": mockOffsetResponse1,
		"FetchRequest":  sarama.NewMockFetchResponse(c, 1),
	})

	// launch test goroutines
	config := sarama.NewConfig()
	config.Consumer.Retry.Backoff = 50 * time.Millisecond
	f, err := NewConsumer([]string{seedBroker.Addr()}, config)
	c.Assert(err, IsNil)

	// we expect to end up (eventually) consuming exactly ten messages on each partition
	var wg sync.WaitGroup
	for i := int32(0); i < 2; i++ {
		pc, _, err := f.ConsumePartition("my_topic", i, 0)
		c.Assert(err, IsNil)

		go func(pc PartitionConsumer) {
			for err := range pc.Errors() {
				c.Error(err)
			}
		}(pc)

		wg.Add(1)
		go func(partition int32, pc PartitionConsumer) {
			for i := 0; i < 10; i++ {
				message := <-pc.Messages()
				c.Assert(message.Offset, Equals, int64(i), Commentf("Incorrect message offset!", i, partition, message.Offset))
				c.Assert(message.Partition, Equals, partition, Commentf("Incorrect message partition!"))
			}
			safeClose(c, pc)
			wg.Done()
		}(i, pc)
	}

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 1")
	// Stage 1:
	//   * my_topic/0 -> leader0 serves 4 messages
	//   * my_topic/1 -> leader1 serves 0 messages

	mockFetchResponse := sarama.NewMockFetchResponse(c, 1)
	for i := 0; i < 4; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(i), testMsg)
	}
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse,
	})

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 2")
	// Stage 2:
	//   * leader0 says that it is no longer serving my_topic/0
	//   * seedBroker tells that leader1 is serving my_topic/0 now

	// seed broker tells that the new partition 0 leader is leader1
	seedBroker.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetLeader("my_topic", 0, leader1.BrokerID()).
			SetLeader("my_topic", 1, leader1.BrokerID()),
	})

	// leader0 says no longer leader of partition 0
	fetchResponse := new(sarama.FetchResponse)
	fetchResponse.AddError("my_topic", 0, sarama.ErrNotLeaderForPartition)
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse),
	})

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 3")
	// Stage 3:
	//   * my_topic/0 -> leader1 serves 6 messages
	//   * my_topic/1 -> leader1 server 8 messages

	// leader1 provides 3 message on partition 0, and 8 messages on partition 1
	mockFetchResponse2 := sarama.NewMockFetchResponse(c, 2)
	for i := 4; i < 10; i++ {
		mockFetchResponse2.SetMessage("my_topic", 0, int64(i), testMsg)
	}
	for i := 0; i < 8; i++ {
		mockFetchResponse2.SetMessage("my_topic", 1, int64(i), testMsg)
	}
	leader1.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse2,
	})

	time.Sleep(50 * time.Millisecond)
	log.Infof("    STAGE 4")
	// Stage 4:
	//   * my_topic/1 -> leader1 tells that it is no longer the leader
	//   * seedBroker tells that leader0 is a new leader for my_topic/1

	// metadata assigns 0 to leader1 and 1 to leader0
	seedBroker.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetLeader("my_topic", 0, leader1.BrokerID()).
			SetLeader("my_topic", 1, leader0.BrokerID()),
	})

	// leader1 says no longer leader of partition1
	fetchResponse4 := new(sarama.FetchResponse)
	fetchResponse4.AddError("my_topic", 1, sarama.ErrNotLeaderForPartition)
	leader1.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": sarama.NewMockWrapper(fetchResponse4),
	})

	// leader0 provides two messages on partition 1
	mockFetchResponse4 := sarama.NewMockFetchResponse(c, 2)
	for i := 8; i < 10; i++ {
		mockFetchResponse4.SetMessage("my_topic", 1, int64(i), testMsg)
	}
	leader0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse4,
	})

	wg.Wait()
	safeClose(c, f)
	leader1.Close()
	leader0.Close()
	seedBroker.Close()
}

// When two partitions have the same broker as the leader, if one partition
// consumer channel buffer is full then that does not affect the ability to
// read messages by the other consumer.
func (s *PartitionConsumerSuite) TestInterleavedClose(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()).
			SetLeader("my_topic", 1, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 1000).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 1100).
			SetOffset("my_topic", 1, sarama.OffsetOldest, 2000).
			SetOffset("my_topic", 1, sarama.OffsetNewest, 2100),
		"FetchRequest": sarama.NewMockFetchResponse(c, 1).
			SetMessage("my_topic", 0, 1000, testMsg).
			SetMessage("my_topic", 0, 1001, testMsg).
			SetMessage("my_topic", 0, 1002, testMsg).
			SetMessage("my_topic", 1, 2000, testMsg),
	})

	config := sarama.NewConfig()
	config.ChannelBufferSize = 0
	f, err := NewConsumer([]string{broker0.Addr()}, config)
	c.Assert(err, IsNil)

	pc0, _, err := f.ConsumePartition("my_topic", 0, 1000)
	c.Assert(err, IsNil)

	pc1, _, err := f.ConsumePartition("my_topic", 1, 2000)
	c.Assert(err, IsNil)

	// When/Then: we can read from partition 0 even if nobody reads from partition 1
	c.Assert((<-pc0.Messages()).Offset, Equals, int64(1000))
	c.Assert((<-pc0.Messages()).Offset, Equals, int64(1001))
	c.Assert((<-pc0.Messages()).Offset, Equals, int64(1002))

	safeClose(c, pc1)
	safeClose(c, pc0)
	safeClose(c, f)
	broker0.Close()
}

func (s *PartitionConsumerSuite) TestBounceWithReferenceOpen(c *C) {
	broker0 := sarama.NewMockBroker(c, 0)
	broker0Addr := broker0.Addr()
	broker1 := sarama.NewMockBroker(c, 1)

	mockMetadataResponse := sarama.NewMockMetadataResponse(c).
		SetBroker(broker0.Addr(), broker0.BrokerID()).
		SetBroker(broker1.Addr(), broker1.BrokerID()).
		SetLeader("my_topic", 0, broker0.BrokerID()).
		SetLeader("my_topic", 1, broker1.BrokerID())

	mockOffsetResponse := sarama.NewMockOffsetResponse(c).
		SetOffset("my_topic", 0, sarama.OffsetOldest, 1000).
		SetOffset("my_topic", 0, sarama.OffsetNewest, 1100).
		SetOffset("my_topic", 1, sarama.OffsetOldest, 2000).
		SetOffset("my_topic", 1, sarama.OffsetNewest, 2100)

	mockFetchResponse := sarama.NewMockFetchResponse(c, 1)
	for i := 0; i < 10; i++ {
		mockFetchResponse.SetMessage("my_topic", 0, int64(1000+i), testMsg)
		mockFetchResponse.SetMessage("my_topic", 1, int64(2000+i), testMsg)
	}

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"OffsetRequest": mockOffsetResponse,
		"FetchRequest":  mockFetchResponse,
	})
	broker1.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": mockMetadataResponse,
		"OffsetRequest":   mockOffsetResponse,
		"FetchRequest":    mockFetchResponse,
	})

	config := sarama.NewConfig()
	config.Consumer.Return.Errors = true
	config.Consumer.Retry.Backoff = 100 * time.Millisecond
	config.ChannelBufferSize = 1
	f, err := NewConsumer([]string{broker1.Addr()}, config)
	c.Assert(err, IsNil)

	pc0, _, err := f.ConsumePartition("my_topic", 0, 1000)
	c.Assert(err, IsNil)

	pc1, _, err := f.ConsumePartition("my_topic", 1, 2000)
	c.Assert(err, IsNil)

	// read messages from both partition to make sure that both brokers operate
	// normally.
	c.Assert((<-pc0.Messages()).Offset, Equals, int64(1000))
	c.Assert((<-pc1.Messages()).Offset, Equals, int64(2000))

	// Simulate broker shutdown. Note that metadata response does not change,
	// that is the leadership does not move to another broker. So partition
	// consumer will keep retrying to restore the connection with the broker.
	broker0.Close()

	// Make sure that while the partition/0 leader is down, consumer/partition/1
	// is capable of pulling messages from broker1.
	for i := 1; i < 7; i++ {
		c.Assert((<-pc1.Messages()).Offset, Equals, int64(2000+i))
	}

	// Bring broker0 back to service.
	broker0 = sarama.NewMockBrokerAddr(c, 0, broker0Addr)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"FetchRequest": mockFetchResponse,
	})

	// Read the rest of messages from both partitions.
	for i := 7; i < 10; i++ {
		c.Assert((<-pc1.Messages()).Offset, Equals, int64(2000+i))
	}
	for i := 1; i < 10; i++ {
		c.Assert((<-pc0.Messages()).Offset, Equals, int64(1000+i))
	}

	select {
	case <-pc0.Errors():
	default:
		c.Errorf("Partition consumer should have detected broker restart")
	}

	safeClose(c, pc1)
	safeClose(c, pc0)
	safeClose(c, f)
	broker0.Close()
	broker1.Close()
}

func (s *PartitionConsumerSuite) TestOffsetOutOfRange(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 2)
	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 2000).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 1000),
	})

	f, err := NewConsumer([]string{broker0.Addr()}, nil)
	c.Assert(err, IsNil)

	// When/Then
	pc, offset, err := f.ConsumePartition("my_topic", 0, 0)
	c.Assert(err, IsNil)
	c.Assert(offset, Equals, int64(1000))
	safeClose(c, pc)

	pc, offset, err = f.ConsumePartition("my_topic", 0, 3456)
	c.Assert(err, IsNil)
	c.Assert(offset, Equals, int64(2000))
	safeClose(c, pc)

	safeClose(c, f)
	broker0.Close()
}

// When a master consumer is closed all kittens gets killed.
func (s *PartitionConsumerSuite) TestClose(c *C) {
	// Given
	broker0 := sarama.NewMockBroker(c, 0)
	defer broker0.Close()

	broker0.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(c).
			SetBroker(broker0.Addr(), broker0.BrokerID()).
			SetLeader("my_topic", 0, broker0.BrokerID()),
		"OffsetRequest": sarama.NewMockOffsetResponse(c).
			SetOffset("my_topic", 0, sarama.OffsetNewest, 100).
			SetOffset("my_topic", 0, sarama.OffsetOldest, 1),
	})

	config := sarama.NewConfig()
	config.Net.ReadTimeout = 500 * time.Millisecond
	f, err := NewConsumer([]string{broker0.Addr()}, config)
	c.Assert(err, IsNil)

	// The mock broker is configured not to reply to FetchRequest's. That will
	// make some internal goroutine block for `Config.Net.ReadTimeout`.
	_, _, _ = f.ConsumePartition("my_topic", 0, sarama.OffsetNewest)

	// When/Then: close the consumer while an internal broker consumer is
	// waiting for a response.
	safeClose(c, f)
}

func safeClose(t sarama.TestReporter, c io.Closer) {
	err := c.Close()
	if err != nil {
		t.Error(err)
	}
}
