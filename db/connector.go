package db

import (
	"strconv"
	"strings"
	"time"

	"github.com/nitishm/go-rejson"
	"github.com/go-redis/redis"

	"github.com/techx/playground/config"
)

const ingestClientName string = "ingest"

var (
	instance *redis.Client
	rh *rejson.Handler
)

func Init() {
	config := config.GetConfig()
	rh = rejson.NewReJSONHandler()

	instance = redis.NewClient(&redis.Options{
		Addr: config.GetString("db.addr"),
		Password: config.GetString("db.password"),
		DB: config.GetInt("db.db"),
	})

	rh.SetGoRedisClient(instance)
}

func GetInstance() *redis.Client {
	return instance
}

func GetRejsonHandler() *rejson.Handler {
	return rh
}

func ListenForUpdates(callback func(msg []byte)) {
	// TODO (#1): Think about subscribing to channels corresponding with other
	// ingest servers, but don't subscribe to our own, and send out events
	// from this server when they are first published
	psc := instance.Subscribe("room")

	for {
		msg, err := psc.ReceiveMessage()

		if err != nil {
			// Stop server if we disconnect from Redis
			panic(err)
		}

		if msg.Channel != "room" {
			// Right now we only receive room updates
			continue
		}

		callback([]byte(msg.Payload))
	}
}

func MonitorLeader() {
	// Set our name so we can identify this client as an ingest
	cmd := redis.NewStringCmd("client", "setname", ingestClientName)
	instance.Process(cmd)

	for range time.NewTicker(time.Second).C {
		// Get list of clients connected to Redis
		clientsRes, _ := instance.ClientList().Result()

		// The leader is the first client -- the oldest connection
		clients := strings.Split(clientsRes, "\n")

		var leaderID int

		for _, client := range clients {
			clientParts := strings.Split(client, " ")

			if len(clientParts) < 4 {
				// Probably a newline, invalid client
				continue
			}

			clientName := strings.Split(clientParts[3], "=")[1]

			if clientName != ingestClientName {
				// This redis client is something else, probably redis-cli
				continue
			}

			leaderID, _ = strconv.Atoi(strings.Split(clientParts[0], "=")[1])

			// We found the leader, break
			break
		}

		// Get current ingest ID
		ingestID, _ := instance.ClientID().Result()

		// If we're not the leader, don't do any leader actions
		if leaderID != int(ingestID) {
			continue
		}

		// TODO: (#2) Take care of song ended packets here
	}
}
