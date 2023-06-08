package main

import (
	"encoding/json"
	"fmt"
	"github.com/c-fandango/rocketchat-term/creds"
	"github.com/c-fandango/rocketchat-term/requests"
	"github.com/c-fandango/rocketchat-term/utils"
	"github.com/gorilla/websocket"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"
)

var debugMode = true
var homeDir, _ = os.UserHomeDir()
var cachePath = homeDir + "/.rocketchat-term"

type userSchema struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

type timestampSchema struct {
	TS int `json:"$date"`
}

type messageSchema struct {
	ID       string          `json:"_id"`
	RoomID   string          `json:"rid"`
	Content  string          `json:"msg"`
	SentTS   timestampSchema `json:"ts"`
	UpdateTS timestampSchema `json:"_updatedAt"`
	Sender   userSchema      `json:"u"`
}

type roomSchema struct {
	ID        string   `json:"_id"`
	ReadOnly  bool     `json:"ro"`
	Name      string   `json:"name"`
	Fname     string   `json:"fname"`
	Topic     string   `json:"topic"`
	Usernames []string `json:"usernames"`
	Messages  []messageSchema
}

func (r *roomSchema) makeName() {
	if r.Topic != "" {
		r.Name = r.Topic
	} else if r.Fname != "" {
		r.Name = r.Fname
	} else if r.Name == "" {
		r.Name = initialiseNames(r.Usernames)
	}
}

type errorResponse struct {
	Error   int    `json:"error"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type wssRequest struct {
	ID      string `json:"id"`
	Message string `json:"msg"`
	Method  string `json:"method"`
	Name    string `json:"name"`
}

type wssResponse struct {
	ID         string        `json:"id"`
	Message    string        `json:"msg"`
	Error      errorResponse `json:"error"`
	Collection string        `json:"collection"`
}

type authResponse struct {
	wssResponse
	Result struct {
		User    string          `json:"id"`
		Token   string          `json:"token"`
		Type    string          `json:"type"`
		Expires timestampSchema `json:"tokenExpires"`
	} `json:"result"`
	host string
}

func (a *authResponse) authenticateLdap(username string, password string) string {

	type ldapParams struct {
		Ldap        bool              `json:"ldap"`
		Username    string            `json:"username"`
		LdapPass    string            `json:"ldapPass"`
		LdapOptions map[string]string `json:"ldapOptions"`
	}

	a.ID = utils.RandID(5)

	request := struct {
		wssRequest
		Params []ldapParams `json:"params"`
	}{
		wssRequest: wssRequest{
			ID:      a.ID,
			Message: "method",
			Method:  "login",
		},
		Params: []ldapParams{
			ldapParams{
				Ldap:        true,
				Username:    username,
				LdapPass:    password,
				LdapOptions: map[string]string{},
			},
		}}

	message, _ := json.Marshal(request)

	return string(message)
}

func (a *authResponse) authenticateToken(token string) string {

	a.ID = utils.RandID(5)

	request := struct {
		wssRequest
		Params []map[string]string `json:"params"`
	}{
		wssRequest: wssRequest{
			ID:      a.ID,
			Message: "method",
			Method:  "login",
		},
		Params: []map[string]string{
			map[string]string{"resume": token},
		}}

	message, _ := json.Marshal(request)

	return string(message)
}

func (a *authResponse) handleResponse(response []byte) error {
	json.Unmarshal(response, a)

	if a.Error != (errorResponse{}) {
		creds.ClearCache(cachePath)
		return fmt.Errorf("authorisation failed")
	}

	tokenCache := map[string]string{
		"host":      a.host,
		"user":      a.Result.User,
		"token":     a.Result.Token,
		"expiresAt": strconv.Itoa(a.Result.Expires.TS),
	}

	cache, _ := json.Marshal(tokenCache)

	err := creds.WriteCache(cachePath, cache)

	return err
}

type rooms []roomSchema

func (r rooms) fetchRooms() error {

	params := make([]map[string]string, 0)

	response, err := requests.GetRequest(`/api/v1/rooms.get`, params)

	log.Println(string(response))

	if err != nil {
		return err
	}

	roomResult := struct {
		Rooms []roomSchema `json:"update"`
	}{}
	json.Unmarshal(response, roomResult)

	for i, _ := range roomResult.Rooms {
		r[i].makeName()
	}

	return nil
}

func (r rooms) addMessage(message messageSchema) (roomSchema, error) {
	if message.RoomID == "" {
		return roomSchema{}, fmt.Errorf("message has no room id")
	}
	for i, room := range r {
		if room.ID == message.RoomID {
			r[i].Messages = append(r[i].Messages, message)
			return room, nil
		}
	}
	return roomSchema{}, fmt.Errorf("failed to match room")
}

func (r rooms) fetchNewRoom(roomId string) (roomSchema, error) {

	params := []map[string]string{
		map[string]string{
			"roomId": roomId,
		},
	}

	response, err := requests.GetRequest(`/api/v1/rooms.info`, params)

	log.Println(string(response))

	if err != nil {
		return roomSchema{}, err
	}

	roomResult := struct {
		Room roomSchema `json:"room"`
	}{}

	json.Unmarshal(response, &roomResult)

	roomResult.Room.makeName()
	r = append(r, roomResult.Room)

	return roomResult.Room, nil
}

type subscription struct {
	wssResponse
	Fields struct {
		EventName string          `json:"eventName"`
		Messages  []messageSchema `json:"args"`
	} `json:"fields"`
}

func (s *subscription) handleResponse(response []byte, allRooms *rooms) error {
	const newMessageAllowedDelayMS = 400

	json.Unmarshal(response, s)
	for _, message := range s.Fields.Messages {

		if message.UpdateTS.TS > message.SentTS.TS+newMessageAllowedDelayMS {
			continue
		}

		matchedRoom, err := allRooms.addMessage(message)

		if err != nil && err.Error() == "failed to match room" {
			log.Println(err)
			matchedRoom, err = allRooms.fetchNewRoom(message.RoomID)

			if err != nil {
				log.Println(err)
			}
		}

		if message.Content != "" {
			printMessage(matchedRoom.Name, message.Sender.Name, message.Content, message.SentTS.TS)
		}
	}

	if s.Error != (errorResponse{}) {
		return fmt.Errorf("failed to fetch room data")
	}

	return nil
}

func (s *subscription) constructRequest(roomID string) string {
	s.ID = utils.RandID(5)

	request := struct {
		wssRequest
		Params []string `json:"params"`
	}{
		wssRequest: wssRequest{
			ID:      s.ID,
			Message: "sub",
			Name:    "stream-room-messages",
		},
		Params: []string{
			roomID,
			"false",
		},
	}
	message, _ := json.Marshal(request)

	return string(message)
}

func main() {
	fmt.Println("hello world")

	if debugMode {
		log.SetOutput(os.Stdout)
	} else {
		log.SetOutput(ioutil.Discard)
	}

	var auth authResponse

	credentials, err := getCredentials(cachePath)
	if err != nil {
		fmt.Println(err)
	}
	auth.host = credentials["host"]

	u := url.URL{Scheme: "wss", Host: credentials["host"], Path: "/websocket"}
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)

	if err != nil {
		panic("invalid host")
	}

	defer c.Close()

	var allRooms rooms
	var roomSub subscription
	roomSub.Collection = "stream-room-messages"
	connectMessage := `{"msg": "connect","version": "1","support": ["1"]}`
	pongMessage := `{"msg": "pong"}`

	done := make(chan struct{})
	messageOut := make(chan string)
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	go func() {
		defer close(done)
		messageOut <- connectMessage

		// TODO don't busy loop
		for {
			_, response, err := c.ReadMessage()

			if err != nil {
				log.Println("error in reading incoming message ", err)
			}

			log.Println(string(response))
			var data wssResponse
			json.Unmarshal(response, &data)

			if data.Message == "connected" {
				if credentials["token"] != "" {
					messageOut <- auth.authenticateToken(credentials["token"])
				} else {
					messageOut <- auth.authenticateLdap(credentials["username"], credentials["password"])
				}
			} else if data.ID == auth.ID && data.Message == "result" {
				err := auth.handleResponse(response)
				if err != nil {
					fmt.Println(err)
					return
				}

				fmt.Println("authenticated")
				requests.Host = auth.host
				requests.Token = auth.Result.Token
				requests.User = auth.Result.User

				err = allRooms.fetchRooms()

				if err != nil {
					fmt.Println(err)
					return
				}

				messageOut <- roomSub.constructRequest("__my_messages__")

			} else if data.Collection == roomSub.Collection && data.Message == "changed" {
				err := roomSub.handleResponse(response, &allRooms)
				if err != nil {
					fmt.Println(err)
					return
				}

			} else if data.Message == "ping" {
				messageOut <- pongMessage
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		case m := <-messageOut:

			log.Printf("sending message %s", m)

			err := c.WriteMessage(websocket.TextMessage, []byte(m))

			if err != nil {
				fmt.Println("error sending websocket message ", err)
			}
		case <-interrupt:

			err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

			if err != nil {
				fmt.Println("error closing websocket", err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return
		}
	}
}
