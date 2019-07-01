package main

import (
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"

	avro "github.com/elodina/go-avro"
	kavro "github.com/elodina/go-kafka-avro"
)

type consumeCmd struct {
	sync.Mutex

	topic       string
	brokers     []string
	tlsCA       string
	tlsCert     string
	tlsCertKey  string
	offsets     map[int32]interval
	timeout     time.Duration
	verbose     bool
	version     sarama.KafkaVersion
	encodeValue string
	encodeKey   string
	pretty      bool
	group       string

	client        sarama.Client
	consumer      sarama.Consumer
	offsetManager sarama.OffsetManager
	poms          map[int32]sarama.PartitionOffsetManager
}

var offsetResume int64 = -3

type offset struct {
	relative bool
	start    int64
	diff     int64
}

func (cmd *consumeCmd) resolveOffset(o offset, partition int32) (int64, error) {
	if !o.relative {
		return o.start, nil
	}

	var (
		res int64
		err error
	)

	if o.start == sarama.OffsetNewest || o.start == sarama.OffsetOldest {
		if res, err = cmd.client.GetOffset(cmd.topic, partition, o.start); err != nil {
			return 0, err
		}

		if o.start == sarama.OffsetNewest {
			res = res - 1
		}

		return res + o.diff, nil
	} else if o.start == offsetResume {
		if cmd.group == "" {
			return 0, fmt.Errorf("cannot resume without -group argument")
		}
		pom := cmd.getPOM(partition)
		next, _ := pom.NextOffset()
		return next, nil
	}

	return o.start + o.diff, nil
}

type interval struct {
	start offset
	end   offset
}

type consumeArgs struct {
	topic       string
	brokers     string
	tlsCA       string
	tlsCert     string
	tlsCertKey  string
	timeout     time.Duration
	offsets     string
	verbose     bool
	version     string
	encodeValue string
	encodeKey   string
	encodeAvro  bool
	pretty      bool
	group       string
}

func parseOffset(str string) (offset, error) {
	result := offset{}
	re := regexp.MustCompile("(oldest|newest|resume)?(-|\\+)?(\\d+)?")
	matches := re.FindAllStringSubmatch(str, -1)

	if len(matches) == 0 || len(matches[0]) < 4 {
		return result, fmt.Errorf("Could not parse offset [%v]", str)
	}

	startStr := matches[0][1]
	qualifierStr := matches[0][2]
	intStr := matches[0][3]

	var err error
	if result.start, err = strconv.ParseInt(intStr, 10, 64); err != nil && len(intStr) > 0 {
		return result, fmt.Errorf("Invalid offset [%v]", str)
	}

	if len(qualifierStr) > 0 {
		result.relative = true
		result.diff = result.start
		result.start = sarama.OffsetOldest
		if qualifierStr == "-" {
			result.start = sarama.OffsetNewest
			result.diff = -result.diff
		}
	}

	switch startStr {
	case "newest":
		result.relative = true
		result.start = sarama.OffsetNewest
	case "oldest":
		result.relative = true
		result.start = sarama.OffsetOldest
	case "resume":
		result.relative = true
		result.start = offsetResume
	}

	return result, nil
}

func parseOffsets(str string) (map[int32]interval, error) {
	defaultInterval := interval{
		start: offset{relative: true, start: sarama.OffsetOldest},
		end:   offset{start: 1<<63 - 1},
	}

	if len(str) == 0 {
		return map[int32]interval{-1: defaultInterval}, nil
	}

	result := map[int32]interval{}
	for _, partitionInfo := range strings.Split(str, ",") {
		re := regexp.MustCompile("(all|\\d+)?=?([^:]+)?:?(.+)?")
		matches := re.FindAllStringSubmatch(strings.TrimSpace(partitionInfo), -1)
		if len(matches) != 1 || len(matches[0]) < 3 {
			return result, fmt.Errorf("Invalid partition info [%v]", partitionInfo)
		}

		var partition int32
		start := defaultInterval.start
		end := defaultInterval.end
		partitionMatches := matches[0]

		// partition
		partitionStr := partitionMatches[1]
		if partitionStr == "all" || len(partitionStr) == 0 {
			partition = -1
		} else {
			i, err := strconv.Atoi(partitionStr)
			if err != nil {
				return result, fmt.Errorf("Invalid partition [%v]", partitionStr)
			}
			partition = int32(i)
		}

		// start
		if len(partitionMatches) > 2 && len(strings.TrimSpace(partitionMatches[2])) > 0 {
			startStr := strings.TrimSpace(partitionMatches[2])
			o, err := parseOffset(startStr)
			if err == nil {
				start = o
			}
		}

		// end
		if len(partitionMatches) > 3 && len(strings.TrimSpace(partitionMatches[3])) > 0 {
			endStr := strings.TrimSpace(partitionMatches[3])
			o, err := parseOffset(endStr)
			if err == nil {
				end = o
			}
		}

		result[partition] = interval{start, end}
	}

	return result, nil
}

func (cmd *consumeCmd) failStartup(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	failf("use \"kt consume -help\" for more information")
}

func (cmd *consumeCmd) parseArgs(as []string) {
	var (
		err  error
		args = cmd.parseFlags(as)
	)

	envTopic := os.Getenv("KT_TOPIC")
	if args.topic == "" {
		if envTopic == "" {
			cmd.failStartup("Topic name is required.")
			return
		}
		args.topic = envTopic
	}
	cmd.topic = args.topic
	cmd.tlsCA = args.tlsCA
	cmd.tlsCert = args.tlsCert
	cmd.tlsCertKey = args.tlsCertKey
	cmd.timeout = args.timeout
	cmd.verbose = args.verbose
	cmd.pretty = args.pretty
	cmd.version = kafkaVersion(args.version)
	cmd.group = args.group

	if args.encodeAvro {
		args.encodeValue = "avro"
		args.encodeKey = "avro"
	}

	if args.encodeValue != "string" && args.encodeValue != "hex" && args.encodeValue != "base64" && args.encodeValue != "avro" {
		cmd.failStartup(fmt.Sprintf(`unsupported encodevalue argument %#v, only string, hex and base64 are supported.`, args.encodeValue))
		return
	}
	cmd.encodeValue = args.encodeValue

	if args.encodeKey != "string" && args.encodeKey != "hex" && args.encodeKey != "base64" && args.encodeKey != "avro" {
		cmd.failStartup(fmt.Sprintf(`unsupported encodekey argument %#v, only string, hex and base64 are supported.`, args.encodeValue))
		return
	}
	cmd.encodeKey = args.encodeKey

	if (cmd.encodeKey == "avro" || cmd.encodeValue == "avro") && schemaRegistryUrl == "" {
		cmd.failStartup(fmt.Sprintf(`SCHEMA_REGISTRY_URL is not set`))
	}

	envBrokers := os.Getenv("KT_BROKERS")
	if args.brokers == "" {
		if envBrokers != "" {
			args.brokers = envBrokers
		} else {
			args.brokers = "localhost:9092"
		}
	}
	cmd.brokers = strings.Split(args.brokers, ",")
	for i, b := range cmd.brokers {
		if !strings.Contains(b, ":") {
			cmd.brokers[i] = b + ":9092"
		}
	}

	cmd.offsets, err = parseOffsets(args.offsets)
	if err != nil {
		cmd.failStartup(fmt.Sprintf("%s", err))
	}
}

func (cmd *consumeCmd) parseFlags(as []string) consumeArgs {
	var args consumeArgs
	flags := flag.NewFlagSet("consume", flag.ContinueOnError)
	flags.StringVar(&args.topic, "topic", "", "Topic to consume (required).")
	flags.StringVar(&args.brokers, "brokers", "", "Comma separated list of brokers. Port defaults to 9092 when omitted (defaults to localhost:9092).")
	flags.StringVar(&args.tlsCA, "tlsca", "", "Path to the TLS certificate authority file")
	flags.StringVar(&args.tlsCert, "tlscert", "", "Path to the TLS client certificate file")
	flags.StringVar(&args.tlsCertKey, "tlscertkey", "", "Path to the TLS client certificate key file")
	flags.StringVar(&args.offsets, "offsets", "", "Specifies what messages to read by partition and offset range (defaults to all).")
	flags.DurationVar(&args.timeout, "timeout", time.Duration(0), "Timeout after not reading messages (default 0 to disable).")
	flags.BoolVar(&args.verbose, "verbose", false, "More verbose logging to stderr.")
	flags.BoolVar(&args.pretty, "pretty", true, "Control output pretty printing.")
	flags.StringVar(&args.version, "version", "", "Kafka protocol version")
	flags.StringVar(&args.encodeValue, "encodevalue", "string", "Present message value as (string|hex|base64|avro), defaults to string.")
	flags.StringVar(&args.encodeKey, "encodekey", "string", "Present message key as (string|hex|base64|avro), defaults to string.")
	flags.BoolVar(&args.encodeAvro, "avro", false, "Use avro and schema registry. Same as -encodekey avro -encodevalue avro ")
	flags.StringVar(&args.group, "group", "", "Consumer group to use for marking offsets. kt will mark offsets if this arg is supplied.")

	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage of consume:")
		flags.PrintDefaults()
		fmt.Fprintln(os.Stderr, consumeDocString)
	}

	err := flags.Parse(as)
	if err != nil && strings.Contains(err.Error(), "flag: help requested") {
		os.Exit(0)
	} else if err != nil {
		os.Exit(2)
	}

	return args
}

func (cmd *consumeCmd) setupClient() {
	var (
		err error
		usr *user.User
		cfg = sarama.NewConfig()
	)
	cfg.Version = cmd.version
	if usr, err = user.Current(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read current user err=%v", err)
	}
	cfg.ClientID = "kt-consume-" + sanitizeUsername(usr.Username)
	if cmd.verbose {
		fmt.Fprintf(os.Stderr, "sarama client configuration %#v\n", cfg)
	}
	tlsConfig, err := setupCerts(cmd.tlsCert, cmd.tlsCA, cmd.tlsCertKey)
	if err != nil {
		failf("failed to setup certificates err=%v", err)
	}
	if tlsConfig != nil {
		cfg.Net.TLS.Enable = true
		cfg.Net.TLS.Config = tlsConfig
	}

	if cmd.client, err = sarama.NewClient(cmd.brokers, cfg); err != nil {
		failf("failed to create client err=%v", err)
	}
}

func (cmd *consumeCmd) run(args []string) {
	var err error

	cmd.parseArgs(args)

	if cmd.verbose {
		sarama.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	cmd.setupClient()
	cmd.setupOffsetManager()

	if cmd.consumer, err = sarama.NewConsumerFromClient(cmd.client); err != nil {
		failf("failed to create consumer err=%v", err)
	}
	defer logClose("consumer", cmd.consumer)

	partitions := cmd.findPartitions()
	if len(partitions) == 0 {
		failf("Found no partitions to consume")
	}
	defer cmd.closePOMs()

	cmd.consume(partitions)
}

func (cmd *consumeCmd) setupOffsetManager() {
	if cmd.group == "" {
		return
	}

	var err error
	if cmd.offsetManager, err = sarama.NewOffsetManagerFromClient(cmd.group, cmd.client); err != nil {
		failf("failed to create offsetmanager err=%v", err)
	}
}

func (cmd *consumeCmd) consume(partitions []int32) {
	var (
		wg  sync.WaitGroup
		out = make(chan printContext)
	)

	go print(out, cmd.pretty)

	wg.Add(len(partitions))
	for _, p := range partitions {
		go func(p int32) { defer wg.Done(); cmd.consumePartition(out, p) }(p)
	}
	wg.Wait()
}

func (cmd *consumeCmd) consumePartition(out chan printContext, partition int32) {
	var (
		offsets interval
		err     error
		pcon    sarama.PartitionConsumer
		start   int64
		end     int64
		ok      bool
	)

	if offsets, ok = cmd.offsets[partition]; !ok {
		offsets, ok = cmd.offsets[-1]
	}

	if start, err = cmd.resolveOffset(offsets.start, partition); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read start offset for partition %v err=%v\n", partition, err)
		return
	}

	if end, err = cmd.resolveOffset(offsets.end, partition); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read end offset for partition %v err=%v\n", partition, err)
		return
	}

	if pcon, err = cmd.consumer.ConsumePartition(cmd.topic, partition, start); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to consume partition %v err=%v\n", partition, err)
		return
	}

	cmd.partitionLoop(out, pcon, partition, end)
}

type consumedMessage struct {
	Partition int32       `json:"partition"`
	Offset    int64       `json:"offset"`
	Key       interface{} `json:"key"`
	Value     interface{} `json:"value"`
	Timestamp *time.Time  `json:"timestamp,omitempty"`
}

func newConsumedMessage(m *sarama.ConsumerMessage, encodeKey, encodeValue string) consumedMessage {
	result := consumedMessage{
		Partition: m.Partition,
		Offset:    m.Offset,
		Key:       encodeBytes(m.Key, encodeKey),
		Value:     encodeBytes(m.Value, encodeValue),
	}

	if !m.Timestamp.IsZero() {
		result.Timestamp = &m.Timestamp
	}

	return result
}

/*
func getSchemaRegistryUrl() string {
	if url, ok := os.LookupEnv("SCHEMA_REGISTRY_URL"); ok {
		return url
	}

	return "http://platform-schema-registry.apps.dev.mt2.uptake.run"
}
*/

var schemaRegistryUrl = os.Getenv("SCHEMA_REGISTRY_URL")
var ar = kavro.NewKafkaAvroDecoder(schemaRegistryUrl)

func encodeBytes(data []byte, encoding string) interface{} {
	if data == nil {
		return nil
	}

	switch encoding {
	case "hex":
		return hex.EncodeToString(data)
	case "base64":
		return base64.StdEncoding.EncodeToString(data)
	case "avro":
		value, err := ar.Decode(data)
		if err != nil {
			log.Print(err)
			return nil
			//panic(err)
		}
		switch v := value.(type) {
		case *avro.GenericRecord:
			return v.Map()
		default:
			return v
		}
	default:
		return string(data)
	}
}

func (cmd *consumeCmd) closePOMs() {
	cmd.Lock()
	for p, pom := range cmd.poms {
		if err := pom.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close partition offset manager for partition %v err=%v", p, err)
		}
	}
	cmd.Unlock()
}

func (cmd *consumeCmd) getPOM(p int32) sarama.PartitionOffsetManager {
	cmd.Lock()
	if cmd.poms == nil {
		cmd.poms = map[int32]sarama.PartitionOffsetManager{}
	}
	pom, ok := cmd.poms[p]
	if ok {
		cmd.Unlock()
		return pom
	}

	pom, err := cmd.offsetManager.ManagePartition(cmd.topic, p)
	if err != nil {
		cmd.Unlock()
		failf("failed to create partition offset manager err=%v", err)
	}
	cmd.poms[p] = pom
	cmd.Unlock()
	return pom
}

func (cmd *consumeCmd) partitionLoop(out chan printContext, pc sarama.PartitionConsumer, p int32, end int64) {
	defer logClose(fmt.Sprintf("partition consumer %v", p), pc)
	var (
		timer   *time.Timer
		pom     sarama.PartitionOffsetManager
		timeout = make(<-chan time.Time)
	)

	if cmd.group != "" {
		pom = cmd.getPOM(p)
	}

	for {
		if cmd.timeout > 0 {
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(cmd.timeout)
			timeout = timer.C
		}

		select {
		case <-timeout:
			fmt.Fprintf(os.Stderr, "consuming from partition %v timed out after %s\n", p, cmd.timeout)
			return
		case err := <-pc.Errors():
			fmt.Fprintf(os.Stderr, "partition %v consumer encountered err %s", p, err)
			return
		case msg, ok := <-pc.Messages():
			if !ok {
				fmt.Fprintf(os.Stderr, "unexpected closed messages chan")
				return
			}

			m := newConsumedMessage(msg, cmd.encodeKey, cmd.encodeValue)
			ctx := printContext{output: m, done: make(chan struct{})}
			out <- ctx
			<-ctx.done

			if cmd.group != "" {
				pom.MarkOffset(msg.Offset+1, "")
			}

			if end > 0 && msg.Offset >= end {
				return
			}
		}
	}
}

func (cmd *consumeCmd) findPartitions() []int32 {
	var (
		all []int32
		res []int32
		err error
	)
	if all, err = cmd.consumer.Partitions(cmd.topic); err != nil {
		failf("failed to read partitions for topic %v err=%v", cmd.topic, err)
	}

	if _, hasDefault := cmd.offsets[-1]; hasDefault {
		return all
	}

	for _, p := range all {
		if _, ok := cmd.offsets[p]; ok {
			res = append(res, p)
		}
	}

	return res
}

var consumeDocString = `
The values for -topic and -brokers can also be set via environment variables KT_TOPIC and KT_BROKERS respectively.
The values supplied on the command line win over environment variable values.

Offsets can be specified as a comma-separated list of intervals:

  [[partition=start:end],...]

The default is to consume from the oldest offset on every partition for the given topic.

 - partition is the numeric identifier for a partition. You can use "all" to
   specify a default interval for all partitions.

 - start is the included offset where consumption should start.

 - end is the included offset where consumption should end.

The following syntax is supported for each offset:

  (oldest|newest|resume)?(+|-)?(\d+)?

 - "oldest" and "newest" refer to the oldest and newest offsets known for a
   given partition.

 - "resume" can be used in combination with -group.

 - You can use "+" with a numeric value to skip the given number of messages
   since the oldest offset. For example, "1=+20" will skip 20 offset value since
   the oldest offset for partition 1.

 - You can use "-" with a numeric value to refer to only the given number of
   messages before the newest offset. For example, "1=-10" will refer to the
   last 10 offset values before the newest offset for partition 1.

 - Relative offsets are based on numeric values and will not take skipped
   offsets (e.g. due to compaction) into account.

 - Given only a numeric value, it is interpreted as an absolute offset value.

More examples:

To consume messages from partition 0 between offsets 10 and 20 (inclusive).

  0=10:20

To define an interval for all partitions use -1 as the partition identifier:

  all=2:10

You can also override the offsets for a single partition, in this case 2:

  all=1-10,2=5-10

To consume from multiple partitions:

  0=4:,2=1:10,6

This would consume messages from three partitions:

  - Anything from partition 0 starting at offset 4.
  - Messages between offsets 1 and 10 from partition 2.
  - Anything from partition 6.

To start at the latest offset for each partition:

  all=newest:

Or shorter:

  newest:

To consume the last 10 messages:

  newest-10:

To skip the first 15 messages starting with the oldest offset:

  oldest+10:

In both cases you can omit "newest" and "oldest":

  -10:

and

  +10:

Will achieve the same as the two examples above.

`
