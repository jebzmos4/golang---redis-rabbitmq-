package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/kataras/iris"
	"github.com/kataras/iris/middleware/logger"
	"github.com/kataras/iris/middleware/recover"
	"github.com/streadway/amqp"

	// autoloads environment variables
	_ "github.com/joho/godotenv/autoload"
)

var (
	amqpURI      = flag.String("amqp", os.Getenv("AMQP_URI"), "AMQP URI")
	exchangeName = flag.String("exchange", "analytics-fanout-exchange", "Durable AMQP exchange name")
	exchangeType = flag.String("exchange-type", "fanout", "Exchange type - direct|fanout|topic|x-custom")
	routingKey   = flag.String("publisher-key", "smsc-publisher-key", "AMQP routing key")
	bindingKey   = flag.String("consumer-key", "smsc-consumer-key", "AMQP binding key")
	consumerTag  = flag.String("consumer-tag", "smsc-consumer", "AMQP consumer tag (should not be blank)")
)

var redisPool *redis.Pool
var app = iris.New()

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
		panic(fmt.Sprintf("%s: %s", msg, err))
	}
}

func init() {
	flag.Parse()
	initAmqp()
}

var conn *amqp.Connection
var ch *amqp.Channel
var queue amqp.Queue

var publisherQueueName = "smsc_publisher"
var consumerQueueName = "Route_sms_dlr"
var analyticsQueue = "analytics"
var analyticsDataQueue = "analytics-data"
var shortcode = os.Getenv("SHORTCODE")

var (
	redisDatabase, _ = strconv.Atoi(os.Getenv("MONTHLY_CONTENT_COUNT"))
	redisURI         = flag.String("redis-address", os.Getenv("REDIS_URI"), "Address to the Redis server")
	maxConnections   = flag.Int("max-connections", 10, "Max connections to Redis")
)

func init() {
	flag.Parse()
	initRedis()
}

func initRedis() (redis.Conn, error) {
	conn, err := redis.Dial("tcp", *redisURI, redis.DialDatabase(redisDatabase))
	failOnError(err, "Failed to connect to Redis Server")
	log.Println("Successfully connected to redis server")
	return conn, err
}

// This method initializes a rabbitmq connection and creates a channel from it
func initAmqp() {
	var err error

	conn, err = amqp.Dial(*amqpURI)
	failOnError(err, "Failed to connect to RabbitMQ")
	connectionHandler(conn)
	log.Println("Successfully connected to Rabbitmq server")

	if err = createExchangeAndQueue(conn); err != nil {
		failOnError(err, "Failed to close channel")
	}

	ch, err = conn.Channel()
	failOnError(err, "Failed to open a channel")
}

// Creates an exchange and binds the queues for the anaytics data publishing via fanout
func createExchangeAndQueue(conn *amqp.Connection) error {
	var err error
	ch, err = conn.Channel()
	failOnError(err, "Failed to open a channel")

	log.Printf("got Channel, declaring %q Exchange (%q)", *exchangeType, *exchangeName)
	if err := ch.ExchangeDeclare(
		*exchangeName, // name
		*exchangeType, // type
		true,          // durable
		false,         // auto-deleted
		false,         // internal
		false,         // noWait
		nil,           // arguments
	); err != nil {
		failOnError(err, "Failed to declare exchange")
	}

	//Declare and bind queues
	_, err = ch.QueueDeclare(
		publisherQueueName, // name
		true,               // durable
		false,              // delete when unused
		false,              // exclusive
		false,              // no-wait
		nil,                // arguments
	)
	failOnError(err, "Failed to declare a queue")
	if err = ch.QueueBind(
		analyticsQueue,
		*routingKey,
		*exchangeName,
		false,
		nil,
	); err != nil {
		failOnError(err, "Failed to bind a queue")
	}

	_, err = ch.QueueDeclare(
		consumerQueueName, // name
		true,              // durable
		false,             // delete when unused
		false,             // exclusive
		false,             // no-wait
		nil,               // arguments
	)
	failOnError(err, "Failed to declare a queue")

	if err = ch.QueueBind(
		consumerQueueName,
		*bindingKey,
		*exchangeName,
		false,
		nil,
	); err != nil {
		failOnError(err, "Failed to bind a queue")
	}
	return ch.Close()
}

// Rabbit MQ Connection Handler
func connectionHandler(conn *amqp.Connection) {
	blockings := conn.NotifyBlocked(make(chan amqp.Blocking))
	go func() {
		for b := range blockings {
			if b.Active {
				log.Printf("TCP blocked: %q", b.Reason)
			} else {
				log.Printf("TCP unblocked")
			}
		}
	}()

	closes := conn.NotifyClose(make(chan *amqp.Error))
	go func() {
		for b := range closes {
			panic(b)
		}
	}()
}

// Rabbit MQ Channel Handler
func channelHandler(channel *amqp.Channel) {
	closes := channel.NotifyClose(make(chan *amqp.Error))
	go func() {
		for b := range closes {
			panic(b)
		}
	}()

	cancels := channel.NotifyCancel(make(chan string))
	go func() {
		for b := range cancels {
			log.Panicln(b)
		}
	}()

}

//SMSC Consumer
func consumer() bool {
	var route string
	log.Printf("Queue bound to Exchange, starting Consume (consumer tag %q)", *consumerTag)
	deliveries, err := ch.Consume(
		consumerQueueName, // name
		"consumer",        // consumerTag,
		false,             // noAck
		false,             // exclusive
		false,             // noLocal
		false,             // noWait
		nil,               // arguments
	)
	if err != nil {
		panic(err)
	}
	for d := range deliveries {
		log.Printf(
			"got %dB delivery: [%v] %q",
			len(d.Body),
			d.DeliveryTag,
			d.Body,
		)
		body := string(d.Body)

		data := make(map[string]string)
		err := json.Unmarshal([]byte(body), &data)

		if err != nil {
			panic(err)
		}
		if data["bearer"] != "" {
			route = data["bearer"]
		} else {
			route = "sms"
		}
		msisdn := MsisdnSanitizer(data["to"], false)
		if IsMsisdnBlacklisted(msisdn) {
			d.Ack(true)
			data["state"] = "16"
			data["reason"] = "blacklisted number"
			log.Print(msisdn, " is blacklisted")
			pushAnalyticsToQueue(data)
			return false
		}
		rateExceeded := isMonthlyRateExceeded(msisdn, shortcode, redisPool)

		if rateExceeded {
			d.Ack(true)
			data["state"] = "16"
			data["reason"] = "Monthly rate exceeded"
			pushAnalyticsToQueue(data)
			log.Printf("Frequency capping exceeded for %s failed ", msisdn)
			return false
		}
		if data["add_id"] != "" {
			campaignSent := isContentSentOnCampaign(msisdn, data["ad_id"], redisPool)
			if campaignSent {
				d.Ack(true)
				data["state"] = "16"
				data["reason"] = "Duplicate message on campign"
				pushAnalyticsToQueue(data)
				log.Printf("Message is duplicate for %s on %v failed", msisdn, data["data_id"])
				return false
			}
			if strings.ToLower(route) == "sms" {
				var shortcode string
				if data["from"] != "" {
					shortcode = data["from"]
				} else {
					shortcode = "200"
				}
				if shortcode == "" {
					shortcode = os.Getenv("SHORT_CODE")
				}
				params := make(map[string]interface{})
				params["to"] = msisdn
				params["shortcode"] = shortcode
				params["text"] = data["message"]
				params["ad_id"] = data["ad_id"]
				params["zone_id"] = data["zone_id"]

				sendContent(params, false)
				data["time_sent"] = time.Now().Local().Format("2006.01.02 15:04:05")
				trackContentSentToMsisdn(msisdn, params["ad_id"].(string), shortcode)
				queue := "route_sent_" + params["adType"].(string)
				publisher(queue, data)
				log.Println("campaign Sms Message sent to " + msisdn)
			} else {
				shortcode := "200"
				params := make(map[string]interface{})
				params["to"] = msisdn
				params["shortcode"] = shortcode
				params["text"] = data["message"]
				params["ad_id"] = data["ad_id"]
				params["zone_id"] = data["zone_id"]

				sendContent(params, true)
				data["time_sent"] = time.Now().Local().Format("2006.01.02 15:04:05")
				queue := "route_sent_" + params["adType"].(string)
				publisher(queue, data)
				log.Println("Campaign USSD Message sent to " + msisdn)
			}
		} else {
			data["time_sent"] = time.Now().Local().Format("2006.01.02 15:04:05")
			queue := "Route_no_ad"
			publisher(queue, data)
			log.Println("this message does not include ad ID " + msisdn)
		}
		d.Ack(false)
	}
	log.Printf("handle: deliveries channel closed")
	type Message struct {
		Response string
	}
	m := Message{"Successfully Updated"}
	b, err := json.Marshal(m)
	log.Println(b)
	return true
}

func publisher(queue string, data map[string]string) bool {
	//Declare and bind queues
	_, err := ch.QueueDeclare(
		queue, // name
		true,  // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	failOnError(err, "Failed to declare a queue")
	if err = ch.QueueBind(
		queue,
		*routingKey,
		*exchangeName,
		false,
		nil,
	); err != nil {
		failOnError(err, "Failed to bind a queue")
	}
	jsonData, jsonError := json.Marshal(data)
	if jsonError != nil {
		panic(jsonError)
	}
	error := ch.Publish(
		*exchangeName, // exchange
		*routingKey,   // routing key
		false,         // mandatory
		false,         // immediate
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        jsonData,
		})
	failOnError(error, "Failed to publish a message")
	log.Println("data pushed to queue" + queue)
	return true
}

func pushAnalyticsToQueue(queryParams map[string]string) bool {

	dataToSend := make(map[string]interface{})
	dataToSend["bearer"] = queryParams["bearer"]
	dataToSend["ad_id"] = queryParams["ad_id"]
	dataToSend["zone_id"] = queryParams["zone_id"]
	dataToSend["to"] = queryParams["to"]
	dataToSend["message"] = queryParams["text"]
	dataToSend["from"] = queryParams["shortcode"]
	dataToSend["timestamp"] = queryParams["ts"]
	if queryParams["state"] == "8" {
		dataToSend["status"] = 1
	} else {
		dataToSend["status"] = 0
	}

	jsonData, jsonError := json.Marshal(dataToSend)
	if jsonError != nil {
		panic(jsonError)
	}

	err := ch.Publish(
		*exchangeName, // exchange
		*routingKey,   // routing key
		false,         // mandatory
		false,         // immediate
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        jsonData,
		})
	failOnError(err, "Failed to publish a message")
	log.Println("analytics pushed to queue")
	log.Print("Successfully Updated")
	return false
}

func main() {

	app.Logger().SetLevel("info")
	// Optionally, add two built'n handlers
	// that can recover from any http-relative panics
	// and log the requests to the terminal.
	app.Use(recover.New())
	app.Use(logger.New())

	redisPool = redis.NewPool(initRedis, *maxConnections)
	defer redisPool.Close()

	conn := redisPool.Get()
	defer conn.Close()
	_, err := redis.String(conn.Do("PING"))
	if err != nil {
		log.Printf("cannot 'PING' db: %v", err)
	}
	log.Println("Successfully pinged DB")

	consumer()
}
