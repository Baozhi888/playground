package socket

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/techx/playground/config"
	"github.com/techx/playground/db"
	"github.com/techx/playground/db/models"
	"github.com/techx/playground/socket/packet"
	"github.com/techx/playground/utils"

	"github.com/dgrijalva/jwt-go"
	"github.com/go-redis/redis/v7"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi/transport"
	"google.golang.org/api/youtube/v3"
)

// Hub maintains the set of active clients and broadcasts messages to the clients
type Hub struct {
	// Registered clients
	clients map[string]*Client

	// Inbound messages from the clients
	broadcast chan *SocketMessage

	// Register requests from the clients
	register chan *Client

	// Unregister requests from clients
	unregister chan *Client
}

func (h *Hub) Init() *Hub {
	h.broadcast = make(chan *SocketMessage)
	h.register = make(chan *Client)
	h.unregister = make(chan *Client)
	h.clients = map[string]*Client{}
	return h
}

func (h *Hub) disconnectClient(client *Client) {
	if client.character != nil {
		pip := db.GetInstance().Pipeline()
		pip.Del("character:" + client.character.ID + ":active")
		pip.HDel("character:"+client.character.ID, "ingest")
		pip.SRem("ingest:"+db.GetIngestID()+":characters", client.character.ID)
		teammatesCmd := pip.SMembers("character:" + client.character.ID + ":teammates")
		friendsCmd := pip.SMembers("character:" + client.character.ID + ":friends")
		pip.Exec()

		// Remove this client from the room
		room, _ := db.GetInstance().HGet("character:"+client.character.ID, "room").Result()
		db.GetInstance().SRem("room:"+room+":characters", client.character.ID)

		// Notify others that this client left
		leavePacket := packet.NewLeavePacket(client.character, room)
		h.Send(leavePacket)

		// Tell their friends that they're offline now
		teammateIDs, _ := teammatesCmd.Result()
		friendIDs, _ := friendsCmd.Result()

		res := packet.NewStatusPacket(client.character.ID, false)
		data, _ := res.MarshalBinary()

		// TODO: This will not work with multiple ingest servers
		for _, id := range teammateIDs {
			h.SendBytes("character:"+id, data)
		}

		for _, id := range friendIDs {
			h.SendBytes("character:"+id, data)
		}
	}

	delete(h.clients, client.id)
	close(client.send)
}

// Listens for messages from websocket clients
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client.id] = client
		case client := <-h.unregister:
			h.disconnectClient(client)
		case message := <-h.broadcast:
			// Process incoming messages from clients
			h.processMessage(message)
		}
	}
}

// Sends a message to all of our clients
func (h *Hub) Send(msg encoding.BinaryMarshaler) {
	// Send to other ingest servers
	db.Publish(msg)

	// Send to clients connected to this ingest
	data, _ := msg.MarshalBinary()
	h.ProcessRedisMessage(data)
}

func (h *Hub) SendBytes(room string, msg []byte) {
	for id := range h.clients {
		client := h.clients[id]

		if client.character == nil {
			continue
		}

		if room == "*" {
			client.send <- msg
			continue
		}

		if client.character.Room == room {
			client.send <- msg
			continue
		}

		if strings.Contains(room, "character:") && client.character.ID == strings.Split(room, ":")[1] {
			client.send <- msg
			continue
		}

		// TODO: If this send fails, disconnect the client
	}
}

func (h *Hub) sendSponsorQueueUpdate(sponsorID string) {
	pip := db.GetInstance().Pipeline()
	hackerSubscribersCmd := pip.LRange("sponsor:"+sponsorID+":hackerqueue", 0, -1)
	sponsorSubscribersCmd := pip.SMembers("sponsor:" + sponsorID + ":subscribed")
	pip.Exec()

	hackerIDs, _ := hackerSubscribersCmd.Result()
	sponsorIDs, _ := sponsorSubscribersCmd.Result()
	hackerCmds := make([]*redis.StringStringMapCmd, len(hackerIDs))

	pip = db.GetInstance().Pipeline()

	for i, hackerID := range hackerIDs {
		hackerCmds[i] = pip.HGetAll("subscriber:" + hackerID)
	}

	pip.Exec()

	subscribers := make([]*models.QueueSubscriber, len(hackerIDs))

	for i, hackerCmd := range hackerCmds {
		// Populate subscribers array
		subscriberRes, _ := hackerCmd.Result()
		subscribers[i] = new(models.QueueSubscriber)
		utils.Bind(subscriberRes, subscribers[i])
		subscribers[i].ID = hackerIDs[i]

		// Send queue update to each hacker
		hackerUpdatePacket := packet.NewQueueUpdateHackerPacket(sponsorID, i+1, "")
		data, _ := hackerUpdatePacket.MarshalBinary()

		// TODO: Fix with multiple ingests
		h.SendBytes("character:"+hackerIDs[i], data)
	}

	sponsorUpdatePacket := packet.NewQueueUpdateSponsorPacket(subscribers)
	data, _ := sponsorUpdatePacket.MarshalBinary()

	for _, sponsorID := range sponsorIDs {
		// TODO: Fix with multiple ingests
		h.SendBytes("character:"+sponsorID, data)
	}
}

// Processes an incoming message from Redis
func (h *Hub) ProcessRedisMessage(msg []byte) {
	var res map[string]interface{}
	json.Unmarshal(msg, &res)

	switch res["type"] {
	case "message":
		h.SendBytes("character:"+res["to"].(string), msg)

		if res["to"].(string) != res["from"].(string) {
			h.SendBytes("character:"+res["from"].(string), msg)
		}
	case "chat", "move", "leave":
		h.SendBytes(res["room"].(string), msg)
	case "join":
		h.SendBytes(res["character"].(map[string]interface{})["room"].(string), msg)
	case "element_add", "element_delete", "element_update", "hallway_add", "hallway_delete", "hallway_update":
		h.SendBytes(res["room"].(string), msg)
	case "song":
		h.SendBytes("*", msg)
	case "teleport", "teleport_home":
		var p packet.TeleportPacket
		json.Unmarshal(msg, &p)

		leavePacket, _ := packet.NewLeavePacket(p.Character, p.From).MarshalBinary()
		h.SendBytes(p.From, leavePacket)

		joinPacket, _ := packet.NewJoinPacket(p.Character).MarshalBinary()
		h.SendBytes(p.To, joinPacket)
	}
}

// Processes an incoming message
func (h *Hub) processMessage(m *SocketMessage) {
	p, err := packet.ParsePacket(m.msg)

	if err != nil {
		// TODO: Log to Sentry or something -- this should never happen
		fmt.Println(err)
		log.Println("ERROR: Received invalid packet from", m.sender.id, "->", string(m.msg))
		return
	}

	var characterID string
	role := models.Guest

	if m.sender.character != nil {
		characterID = m.sender.character.ID
		role = models.Role(m.sender.character.Role)
	}

	if !p.PermissionCheck(characterID, role) {
		println("no permission")
		return
	}

	switch p := p.(type) {
	case packet.AddEmailPacket:
		var emailsKey string

		switch models.Role(p.Role) {
		case models.SponsorRep:
			emailsKey = "sponsor_emails"
			db.GetInstance().HSet("emailToSponsor", p.Email, p.SponsorID)
		case models.Mentor:
			emailsKey = "mentor_emails"
		case models.Organizer:
			emailsKey = "organizer_emails"
		default:
			break
		}

		if emailsKey == "" {
			return
		}

		db.GetInstance().SAdd(emailsKey, strings.TrimSpace(p.Email))
	case packet.ChatPacket:
		// Check for non-ASCII characters
		if !utils.IsASCII(p.Message) {
			// TODO: Send error packet
			return
		}

		// Publish chat event to other clients
		p.Room = m.sender.character.Room
		p.ID = m.sender.character.ID
		h.Send(p)
	case packet.ElementTogglePacket:
		elementRes, _ := db.GetInstance().HGetAll("element:" + p.ID).Result()
		var element models.Element
		utils.Bind(elementRes, &element)
		element.ID = p.ID

		numStates := strings.Count(element.Path, ",") + 1

		if element.State < numStates-1 {
			element.State = element.State + 1
		} else {
			element.State = 0
		}

		db.GetInstance().HSet("element:"+p.ID, "state", element.State)

		// Publish update to other ingest servers
		update := packet.NewElementUpdatePacket(m.sender.character.Room, p.ID, element)
		h.Send(update)
	case packet.ElementUpdatePacket:
		p.Room = m.sender.character.Room

		if p.Element.Path == "tiles/blue1.svg" {
			p.Element.ChangingImagePath = true
			p.Element.ChangingPaths = "tiles/blue1.svg,tiles/blue2.svg,tiles/blue3.svg,tiles/blue4.svg,tiles/green1.svg,tiles/green2.svg,tiles/pink1.svg,tiles/pink2.svg,tiles/pink3.svg,tiles/pink4.svg,tiles/yellow1.svg"
			p.Element.ChangingInterval = 2000
		}

		if p.Element.Path == "djbooth.svg" {
			p.Element.Action = int(models.OpenJukebox)
		}

		db.GetInstance().HSet("element:"+p.ID, utils.StructToMap(p.Element))

		// Publish event to other ingest servers
		h.Send(p)
	case packet.EmailCodePacket:
		isValidEmail := false

		// Make sure this email exists in our database
		switch models.Role(p.Role) {
		case models.SponsorRep:
			isValidEmail, _ = db.GetInstance().SIsMember("sponsor_emails", p.Email).Result()
		case models.Mentor:
			isValidEmail, _ = db.GetInstance().SIsMember("mentor_emails", p.Email).Result()
		case models.Organizer:
			isValidEmail, _ = db.GetInstance().SIsMember("organizer_emails", p.Email).Result()
		default:
			break
		}

		if !isValidEmail {
			return
		}

		code := rand.Intn(1000000)
		db.GetInstance().SAdd("login_requests", p.Email+","+strconv.Itoa(code))

		// TODO (starter task): Send a nice email to this person with their code
		fmt.Println(code)
	case packet.EventPacket:
		// Parse event packet
		res := packet.EventPacket{}
		json.Unmarshal(m.msg, &res)

		isValidEvent, err := db.GetInstance().SIsMember("events", res.ID).Result()

		if !isValidEvent || err != nil {
			return
		}

		pip := db.GetInstance().Pipeline()
		pip.SAdd("event:"+res.ID+":attendees", m.sender.character.ID)
		pip.SAdd("character:"+m.sender.character.ID+":events", res.ID)
		pip.SCard("character:" + m.sender.character.ID + ":events")
		numEventsCmd := pip.HIncrBy("character:"+m.sender.character.ID+":achievements", "events", 1)
		pip.Exec()

		// Check achievement progress and update if necessary
		numEvents, err := numEventsCmd.Result()

		if numEvents == config.GetConfig().GetInt64("achievements.num_events") && err == nil {
			resp := packet.NewAchievementNotificationPacket("events")
			data, _ := resp.MarshalBinary()
			h.SendBytes("character:"+m.sender.character.ID, data)
		}
	case packet.FriendRequestPacket:
		// Parse friend request packet
		res := packet.FriendRequestPacket{}
		json.Unmarshal(m.msg, &res)

		if res.RecipientID == res.SenderID {
			return
		}

		res.SenderID = m.sender.character.ID

		// Check if the other person has also sent a friend request
		isExistingRequest, _ := db.GetInstance().SIsMember("character:"+m.sender.character.ID+":requests", res.RecipientID).Result()

		if isExistingRequest {
			pip := db.GetInstance().Pipeline()
			pip.SRem("character:"+m.sender.character.ID+":requests", res.RecipientID)
			pip.SAdd("character:"+m.sender.character.ID+":friends", res.RecipientID)
			pip.SAdd("character:"+res.RecipientID+":friends", m.sender.character.ID)
			pip.Exec()

			// TODO: This will not work with more than one ingest server
			firstUpdate := packet.NewFriendUpdatePacket(res.RecipientID, m.sender.character.ID)
			data, _ := firstUpdate.MarshalBinary()
			h.SendBytes("character:"+res.RecipientID, data)

			secondUpdate := packet.NewFriendUpdatePacket(m.sender.character.ID, res.RecipientID)
			data, _ = secondUpdate.MarshalBinary()
			h.SendBytes("character:"+m.sender.character.ID, data)
		} else {
			db.GetInstance().SAdd("character:"+res.RecipientID+":requests", m.sender.character.ID)

			// TODO: This will not work with more than one ingest server
			friendUpdate := packet.NewFriendUpdatePacket(res.RecipientID, m.sender.character.ID)
			data, _ := friendUpdate.MarshalBinary()
			h.SendBytes("character:"+res.RecipientID, data)
		}
	case packet.GetAchievementsPacket:
		// Send achievements back to client
		resp := packet.NewAchievementsPacket(m.sender.character.ID)
		data, _ := resp.MarshalBinary()
		h.SendBytes("character:"+m.sender.character.ID, data)
	case packet.GetMapPacket:
		// Send locations back to client
		resp := packet.NewMapPacket()
		data, _ := resp.MarshalBinary()
		h.SendBytes("character:"+m.sender.character.ID, data)
	case packet.GetMessagesPacket:
		sender := m.sender.character.ID

		ha := fnv.New32a()
		ha.Write([]byte(sender))
		senderHash := ha.Sum32()

		ha.Reset()
		ha.Write([]byte(p.Recipient))
		recipientHash := ha.Sum32()

		conversationKey := "conversation:" + sender + ":" + p.Recipient

		if recipientHash < senderHash {
			conversationKey = "conversation:" + p.Recipient + ":" + sender
		}

		messageIDs, _ := db.GetInstance().LRange(conversationKey, -100, -1).Result()

		pip := db.GetInstance().Pipeline()
		messageCmds := make([]*redis.StringStringMapCmd, len(messageIDs))

		for i, messageID := range messageIDs {
			messageCmds[i] = pip.HGetAll("message:" + messageID)
		}

		pip.Exec()
		messages := make([]*models.Message, len(messageIDs))

		for i, messageCmd := range messageCmds {
			messageRes, _ := messageCmd.Result()
			messages[i] = new(models.Message)
			utils.Bind(messageRes, messages[i])
		}

		resp := packet.NewMessagesPacket(messages, p.Recipient)
		data, _ := resp.MarshalBinary()
		h.SendBytes("character:"+m.sender.character.ID, data)
	case packet.HallwayAddPacket:
		p.Room = m.sender.character.Room
		p.ID = uuid.New().String()

		pip := db.GetInstance().Pipeline()
		pip.HSet("hallway:"+p.ID, utils.StructToMap(p.Hallway))
		pip.SAdd("room:"+p.Room+":hallways", p.ID)
		pip.Exec()

		// Publish event to other ingest servers
		h.Send(p)
	case packet.HallwayDeletePacket:
		p.Room = m.sender.character.Room

		pip := db.GetInstance().Pipeline()
		pip.Del("hallway:" + p.ID)
		pip.SRem("room:"+p.Room+":hallways", p.ID)
		pip.Exec()

		// Publish event to other ingest servers
		h.Send(p)
	case packet.HallwayUpdatePacket:
		p.Room = m.sender.character.Room

		db.GetInstance().HSet("hallway:"+p.ID, utils.StructToMap(p.Hallway))

		// Publish event to other ingest servers
		h.Send(p)
	case packet.JoinPacket:
		// Type auth is used when the character is just connecting to the socket, but not actually
		// joining a room. This is useful in limited circumstances, e.g. recording event attendance

		character := new(models.Character)
		var initPacket *packet.InitPacket
		firstTime := false

		pip := db.GetInstance().Pipeline()

		if p.Name != "" {
			character = models.NewCharacter(p.Name)

			// Add character to database
			character.Ingest = db.GetIngestID()
			db.GetInstance().HSet("character:"+character.ID, utils.StructToMap(character))
		} else if p.QuillToken != "" {
			// Fetch data from Quill
			quillValues := map[string]string{
				"token": p.QuillToken,
			}

			quillBody, _ := json.Marshal(quillValues)
			// TODO: Error handling
			resp, _ := http.Post("https://my.hackmit.org/auth/sso/exchange", "application/json", bytes.NewBuffer(quillBody))

			defer resp.Body.Close()
			body, _ := ioutil.ReadAll(resp.Body)

			var quillData map[string]interface{}
			err := json.Unmarshal(body, &quillData)

			if err != nil {
				// Likely invalid SSO token
				// TODO: Send error packet
				return
			}

			admitted := quillData["status"].(map[string]interface{})["admitted"].(bool)

			if !admitted {
				// Don't allow non-admitted hackers to access Playground
				// TODO: Send error packet
				return
			}

			// Load this client's character
			characterID, err := db.GetInstance().HGet("quillToCharacter", quillData["id"].(string)).Result()

			if err != nil {
				// Never seen this character before, create a new one
				character = models.NewCharacterFromQuill(quillData)
				character.ID = uuid.New().String()

				// Add character to database
				pip.HSet("character:"+character.ID, utils.StructToMap(character))
				pip.HSet("quillToCharacter", quillData["id"].(string), character.ID)
			} else {
				// This person has logged in before, fetch from Redis
				characterRes, _ := db.GetInstance().HGetAll("character:" + characterID).Result()
				utils.Bind(characterRes, &character)
				character.ID = characterID
			}
		} else if p.Token != "" {
			// TODO: Error handling
			token, err := jwt.Parse(p.Token, func(token *jwt.Token) (interface{}, error) {
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
				}

				config := config.GetConfig()
				return []byte(config.GetString("jwt.secret")), nil
			})

			if err != nil {
				errorPacket := packet.NewErrorPacket(1)
				data, _ := json.Marshal(errorPacket)
				m.sender.send <- data
				return
			}

			var characterID string

			if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
				characterID = claims["id"].(string)
			} else {
				// TODO: Error handling
				return
			}

			// This person has logged in before, fetch from Redis
			characterRes, err := db.GetInstance().HGetAll("character:" + characterID).Result()

			if err != nil || len(characterRes) == 0 {
				errorPacket := packet.NewErrorPacket(1)
				data, _ := json.Marshal(errorPacket)
				m.sender.send <- data
				return
			}

			utils.Bind(characterRes, character)
			character.ID = characterID
		} else if p.Email != "" {
			isValidLoginRequest, _ := db.GetInstance().SIsMember("login_requests", p.Email+","+strconv.Itoa(p.Code)).Result()

			if !isValidLoginRequest {
				return
			}

			// Load this client's character
			characterID, err := db.GetInstance().HGet("emailToCharacter", p.Email).Result()

			if err != nil {
				// Never seen this character before, create a new one
				character = models.NewCharacter("Player")
				character.ID = uuid.New().String()

				// Check this character's role
				rolePip := db.GetInstance().Pipeline()
				sponsorCmd := rolePip.SIsMember("sponsor_emails", p.Email)
				sponsorIDCmd := rolePip.HGet("emailToSponsor", p.Email)
				mentorCmd := rolePip.SIsMember("mentor_emails", p.Email)
				organizerCmd := rolePip.SIsMember("organizer_emails", p.Email)
				rolePip.Exec()

				isSponsor, _ := sponsorCmd.Result()
				isMentor, _ := mentorCmd.Result()
				isOrganizer, _ := organizerCmd.Result()

				if isSponsor {
					character.Role = int(models.SponsorRep)

					sponsorID, _ := sponsorIDCmd.Result()
					character.SponsorID = sponsorID
				} else if isMentor {
					character.Role = int(models.Mentor)
				} else if isOrganizer {
					character.Role = int(models.Organizer)
				}

				// Add character to database
				pip.HSet("character:"+character.ID, utils.StructToMap(character))
				pip.HSet("emailToCharacter", p.Email, character.ID)

				// Make sure they get the account setup screen
				firstTime = true
			} else {
				// This person has logged in before, fetch from Redis
				characterRes, _ := db.GetInstance().HGetAll("character:" + characterID).Result()
				utils.Bind(characterRes, &character)
				character.ID = characterID
			}

			p.Email = ""
			p.Code = 0
		} else {
			// Client provided no authentication data
			return
		}

		if p.Type == "join" {
			// Generate init packet before new character is added to room
			initPacket = packet.NewInitPacket(character.ID, character.Room, true)
			initPacket.FirstTime = firstTime

			// Add to whatever room they were in
			pip.SAdd("room:"+character.Room+":characters", character.ID)
		}

		// Add this character's id to this ingest in Redis
		pip.SAdd("ingest:"+character.Ingest+":characters", character.ID)

		character.Ingest = db.GetIngestID()
		pip.HSet("character:"+character.ID, "ingest", db.GetIngestID())

		// Make sure character ID isn't an empty string
		if character.ID == "" {
			fmt.Println("ERROR: Empty character ID on join")
			return
		}

		// Set this character's status to active
		pip.Set("character:"+character.ID+":active", "true", 0)

		// Get info for friends notification
		teammatesCmd := pip.SMembers("character:" + character.ID + ":teammates")
		friendsCmd := pip.SMembers("character:" + character.ID + ":friends")

		// Wrap up
		pip.Exec()

		// Tell their friends that they're online now
		teammateIDs, _ := teammatesCmd.Result()
		friendIDs, _ := friendsCmd.Result()

		statusRes := packet.NewStatusPacket(character.ID, true)
		statusData, _ := statusRes.MarshalBinary()

		// TODO: This will not work with multiple ingest servers
		for _, id := range teammateIDs {
			h.SendBytes("character:"+id, statusData)
		}

		for _, id := range friendIDs {
			h.SendBytes("character:"+id, statusData)
		}

		// Authenticate the user on our end
		m.sender.character = character

		if p.Type == "join" {
			// Make sure SSO token is omitted from join packet that is sent to clients
			p.Name = ""
			p.QuillToken = ""
			p.Token = ""

			// Send them the relevant init packet
			data, _ := initPacket.MarshalBinary()
			m.sender.send <- data

			// Send the join packet to clients and Redis
			p.Character = character

			h.Send(p)
		}
	case packet.MessagePacket:
		// TODO: Save timestamp
		p.From = m.sender.character.ID

		// Check for non-ASCII characters
		if !utils.IsASCII(p.Message.Text) {
			// TODO: Send error packet
			return
		}

		messageID := uuid.New().String()

		pip := db.GetInstance().Pipeline()
		pip.HSet("message:"+messageID, utils.StructToMap(p.Message))

		ha := fnv.New32a()
		ha.Write([]byte(p.From))
		senderHash := ha.Sum32()

		ha.Reset()
		ha.Write([]byte(p.To))
		recipientHash := ha.Sum32()

		conversationKey := "conversation:" + p.From + ":" + p.To

		if recipientHash < senderHash {
			conversationKey = "conversation:" + p.To + ":" + p.From
		}

		pip.RPush(conversationKey, messageID)
		pip.Exec()

		h.Send(p)
	case packet.MovePacket:
		if m.sender.character == nil {
			return
		}

		// Update character's position in the room
		pip := db.GetInstance().Pipeline()
		pip.HSet("character:"+m.sender.character.ID, "x", p.X)
		pip.HSet("character:"+m.sender.character.ID, "y", p.Y)
		_, err := pip.Exec()

		if err != nil {
			log.Println(err)
			log.Fatal("ERROR: Failure sending move packet to Redis")
			return
		}

		// Publish move event to other ingest servers
		p.Room = m.sender.character.Room
		p.ID = m.sender.character.ID

		h.Send(p)

		// notif := packet.NewMessageNotificationPacket("testing")
		// data, _ := notif.MarshalBinary()
		// h.SendBytes("character:"+m.sender.character.ID, data)
	case packet.RegisterPacket:
		pip := db.GetInstance().Pipeline()

		if p.Name != "" {
			pip.HSet("character:"+m.sender.character.ID, "name", p.Name)
		}

		if p.PhoneNumber != "" {
			pip.HSet("character:"+m.sender.character.ID+":settings", "phoneNumber", p.PhoneNumber)
		}

		if p.BrowserSubscription != nil {
			resp, err := webpush.SendNotification([]byte("Test"), p.BrowserSubscription, &webpush.Options{
				Subscriber:      "jbcook418@gmail.com",
				VAPIDPublicKey:  config.GetConfig().GetString("webpush.public_key"),
				VAPIDPrivateKey: config.GetConfig().GetString("webpush.private_key"),
				TTL:             30,
			})

			if err != nil {
				fmt.Println("Error sending browser notification:")
				fmt.Println(err)
			}

			defer resp.Body.Close()
		}

		roomCmd := pip.HGet("character:"+m.sender.character.ID, "room")
		pip.Exec()
		room, _ := roomCmd.Result()

		initPacket := packet.NewInitPacket(m.sender.character.ID, room, true)
		data, _ := initPacket.MarshalBinary()
		h.SendBytes("character:"+m.sender.character.ID, data)
	case packet.SettingsPacket:
		db.GetInstance().HSet("character:"+m.sender.character.ID+":settings", utils.StructToMap(p.Settings))
		h.SendBytes("character:"+m.sender.character.ID, m.msg)
	case packet.SongPacket:
		// Make the YouTube API call
		youtubeClient, _ := youtube.New(&http.Client{
			Transport: &transport.APIKey{Key: config.GetSecret(config.YouTubeKey)},
		})

		call := youtubeClient.Videos.List([]string{"snippet", "contentDetails"}).
			Id(p.VidCode)

		response, err := call.Do()
		if err != nil {
			// TODO: Send error packet
			panic(err)
		}

		// Should only have one video
		for _, video := range response.Items {
			// Parse duration string
			duration := video.ContentDetails.Duration
			minIndex := strings.Index(duration, "M")
			secIndex := strings.Index(duration, "S")

			// Convert duration to seconds
			minutes, err := strconv.Atoi(duration[2:minIndex])
			seconds, err := strconv.Atoi(duration[minIndex+1 : secIndex])

			// Error parsing duration string
			if err != nil {
				// TODO: Send error packet
				panic(err)
			}

			p.Duration = (minutes * 60) + seconds
			p.Title = video.Snippet.Title
			p.ThumbnailURL = video.Snippet.Thumbnails.Default.Url
		}

		songID := uuid.New().String()

		pip := db.GetInstance().Pipeline()
		pip.HSet("song:"+songID, utils.StructToMap(p.Song))
		pip.RPush("songs", songID)
		pip.Exec()

		if err != nil {
			// TODO: Send error packet
			panic(err)
		}

		h.Send(p)
	case packet.StatusPacket:
		if m.sender.character == nil {
			return
		}

		p.ID = m.sender.character.ID
		p.Online = true

		pip := db.GetInstance().Pipeline()

		if p.Active {
			pip.Set("character:"+m.sender.character.ID+":active", "true", 0)
		} else {
			pip.Set("character:"+m.sender.character.ID+":active", "false", 0)
		}

		teammatesCmd := pip.SMembers("character:" + m.sender.character.ID + ":teammates")
		friendsCmd := pip.SMembers("character:" + m.sender.character.ID + ":friends")
		pip.Exec()

		teammateIDs, _ := teammatesCmd.Result()
		friendIDs, _ := friendsCmd.Result()

		data, _ := p.MarshalBinary()

		// TODO: This will not work with multiple ingest servers
		for _, id := range teammateIDs {
			h.SendBytes("character:"+id, data)
		}

		for _, id := range friendIDs {
			h.SendBytes("character:"+id, data)
		}
	case packet.TeleportPacket:
		p.From = m.sender.character.Room

		if p.X <= 0 || p.X >= 1 {
			p.X = 0.5
		}

		if p.Y <= 0 || p.Y >= 1 {
			p.Y = 0.5
		}

		pip := db.GetInstance().Pipeline()

		if p.Type == "teleport_home" {
			p.From = m.sender.character.Room

			if m.sender.character.SponsorID != "" {
				// If this character is a sponsor rep, send them to their sponsor room
				p.To = "sponsor:" + m.sender.character.SponsorID
			} else {
				// Otherwise, send them to their personal room
				homeExists, _ := db.GetInstance().SIsMember("rooms", "home:"+m.sender.character.ID).Result()

				if !homeExists {
					db.CreateRoom("home:"+m.sender.character.ID, db.Personal)
				}

				p.To = "home:" + m.sender.character.ID
			}
		}

		// Update this character's room
		pip.HSet("character:"+m.sender.character.ID, map[string]interface{}{
			"room": p.To,
			"x":    p.X,
			"y":    p.Y,
		})

		// Remove this character from the previous room
		pip.SRem("room:"+m.sender.character.Room+":characters", m.sender.character.ID)
		pip.Exec()

		// Send them the init packet for this room
		initPacket := packet.NewInitPacket(m.sender.character.ID, p.To, false)
		initPacketData, _ := initPacket.MarshalBinary()
		m.sender.send <- initPacketData
		m.sender.character.Room = p.To

		// Add them to their new room
		pip = db.GetInstance().Pipeline()
		characterCmd := pip.HGetAll("character:" + m.sender.character.ID)
		pip.SAdd("room:"+p.To+":characters", m.sender.character.ID)
		pip.Exec()

		characterRes, _ := characterCmd.Result()
		var character models.Character
		utils.Bind(characterRes, &character)
		character.ID = m.sender.character.ID

		// Publish event to other ingest servers
		p.Character = &character
		h.Send(p)
	case packet.QueueJoinPacket:
		hackerIDs, _ := db.GetInstance().LRange("sponsor:"+p.SponsorID+":hackerqueue", 0, -1).Result()

		for _, hackerID := range hackerIDs {
			if hackerID == m.sender.character.ID {
				// This hacker is already in the queue
				return
			}
		}

		pip := db.GetInstance().Pipeline()
		pip.RPush("sponsor:"+p.SponsorID+":hackerqueue", m.sender.character.ID)
		pip.HSet("character:"+m.sender.character.ID, "queueId", p.SponsorID)

		subscriber := models.NewQueueSubscriber(m.sender.character)
		pip.HSet("subscriber:"+m.sender.character.ID, utils.StructToMap(subscriber))
		pip.Exec()

		h.sendSponsorQueueUpdate(p.SponsorID)
	case packet.QueueRemovePacket:
		pip := db.GetInstance().Pipeline()
		pip.LRem("sponsor:"+p.SponsorID+":hackerqueue", 0, p.CharacterID)
		pip.HSet("character:"+p.CharacterID, "queueId", "")
		pip.Exec()

		h.sendSponsorQueueUpdate(p.SponsorID)

		if m.sender.character.Role == int(models.SponsorRep) {
			// If a sponsor took a hacker off the queue, send them the sponsor's URL
			// TODO: Replace this with the sponsor's actual URL
			hackerUpdatePacket := packet.NewQueueUpdateHackerPacket(p.SponsorID, 0, "https://google.com")
			data, _ := hackerUpdatePacket.MarshalBinary()
			h.SendBytes("character:"+p.CharacterID, data)
		}
	case packet.QueueSubscribePacket:
		db.GetInstance().SAdd("sponsor:"+p.SponsorID+":subscribed", m.sender.character.ID)

		// TODO: This is inefficient, we should just send the update to the newly subscribed sponsor
		h.sendSponsorQueueUpdate(p.SponsorID)
	case packet.QueueUnsubscribePacket:
		db.GetInstance().SRem("sponsor:"+p.SponsorID+":subscribed", m.sender.character.ID)
	case packet.UpdateMapPacket:
		// Update this character's location
		locationID := m.sender.character.ID

		pip := db.GetInstance().Pipeline()
		pip.HSet("location:"+locationID, utils.StructToMap(p.Location))
		pip.SAdd("locations", locationID)
		pip.Exec()

		// Send locations back to client
		resp := packet.NewMapPacket()
		data, _ := resp.MarshalBinary()
		h.SendBytes("character:"+m.sender.character.ID, data)
	}
}
