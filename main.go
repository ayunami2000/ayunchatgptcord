package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EagleChen/mapmutex"
	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/diamondburned/arikawa/v3/session"
	"github.com/diamondburned/arikawa/v3/utils/json/option"
)

var s *session.Session
var botID discord.UserID
var muti = mapmutex.NewMapMutex()
var threads = make(map[discord.ChannelID][]ChatMessage)
var ctx context.Context
var inactiveTime = time.Minute * 10
var maxTokens = uint(256)
var threadCount = make(map[discord.GuildID]*ThreadCountMapWithLock)
var maxThreads = uint(3)

type ThreadCountWithLock struct {
	Count uint
	Lock  sync.Mutex
}

type ThreadCountMapWithLock struct {
	Map  map[discord.UserID]*ThreadCountWithLock
	Lock sync.Mutex
}

var TOKEN = os.Getenv("BOT_TOKEN")
var APIKEY = os.Getenv("APIKEY")
var ENDPOINT = os.Getenv("ENDPOINT")
var INACTIVE_TIME = os.Getenv("INACTIVE_TIME")
var MAX_TOKENS = os.Getenv("MAX_TOKENS")
var MAX_THREADS = os.Getenv("MAX_THREADS")
var SPLIT_STR_REGEX = regexp.MustCompile(`(?s)(?:.{1,1000}.{0,1000}(?:$|\s|\n)|.{1000}.{1000})`)

func extractUserIDFromThreadName(name string) (discord.UserID, error) {
	trimOne := name[10:]
	trimOne = trimOne[:strings.Index(trimOne, " ")]
	sf, err := discord.ParseSnowflake(trimOne)
	if err != nil {
		log.Println(123)
		return discord.NullUserID, errors.New("could not parse user ID from thread name")
	}
	return discord.UserID(sf), nil
}

func addToThreadCount(guildID discord.GuildID, userID discord.UserID, amount int) error {
	if threadCount[guildID] == nil {
		threadCount[guildID] = &ThreadCountMapWithLock{Map: make(map[discord.UserID]*ThreadCountWithLock)}
	}
	threadCount[guildID].Lock.Lock()
	if threadCount[guildID].Map[userID] == nil {
		threadCount[guildID].Map[userID] = &ThreadCountWithLock{Count: 0}
	}
	threadCount[guildID].Map[userID].Lock.Lock()
	newVal := uint(int(threadCount[guildID].Map[userID].Count) + amount)
	if newVal > maxThreads {
		threadCount[guildID].Map[userID].Lock.Unlock()
		threadCount[guildID].Lock.Unlock()
		return errors.New("too many threads")
	}
	threadCount[guildID].Map[userID].Count = newVal
	flag := threadCount[guildID].Map[userID].Count == 0
	threadCount[guildID].Map[userID].Lock.Unlock()
	if flag {
		delete(threadCount[guildID].Map, userID)
		flag = len(threadCount[guildID].Map) == 0
	}
	threadCount[guildID].Lock.Unlock()
	if flag {
		delete(threadCount, guildID)
	}
	return nil
}

func Post(url, contentType string, body io.Reader) (resp *http.Response, err error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+APIKEY)
	return http.DefaultClient.Do(req)
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model            string        `json:"model"`
	Messages         []ChatMessage `json:"messages"`
	Temperature      float64       `json:"temperature,omitempty"`
	MaxTokens        uint          `json:"max_tokens,omitempty"`
	TopP             float64       `json:"top_p,omitempty"`
	FrequencyPenalty float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  float64       `json:"presence_penalty,omitempty"`
	User             string        `json:"user,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
}

func interactionCreateEvent(e *gateway.InteractionCreateEvent) {
	var resp api.InteractionResponse
	basicResp := "**Error:** Unknown error!"

	switch data := e.Data.(type) {
	case *discord.CommandInteraction:
		switch data.Name {
		case "initbuttons":
			resp = api.InteractionResponse{
				Type: api.MessageInteractionWithSource,
				Data: &api.InteractionResponseData{
					Content: option.NewNullableString("**Welcome!** Press the button below to create a new thread.\n- Each user can have up to **" + strconv.Itoa(int(maxThreads)) + "** threads at a time.\n- Threads will be automatically deleted after **" + inactiveTime.String() + "** of inactivity.\n- **Invite** people to your thread with `/invite`!\n- **Remove** people from your thread with `/remove`!\n- **Delete** your thread with `/delete`!"),
					Components: discord.ComponentsPtr(
						&discord.ActionRowComponent{
							&discord.ButtonComponent{
								Label:    "Start a thread!",
								CustomID: "create",
								Emoji:    &discord.ComponentEmoji{Name: "💬"},
								Style:    discord.PrimaryButtonStyle(),
							},
						},
					),
				},
			}
		case "invite":
			ch, err := s.Channel(e.ChannelID)
			if err != nil {
				basicResp = "**Error:** Could not get channel!"
				log.Println("could not get channel:", err)
				break
			}
			if ch.Type != discord.GuildPrivateThread {
				basicResp = "**Error:** This command can only be used in threads!"
				break
			}

			sn, err := data.Options.Find("user").SnowflakeValue()
			if err != nil {
				basicResp = "**Error:** User not found!"
			} else {
				targetUser := discord.UserID(sn)
				basicResp = "**Inviting user!**"
				err = s.AddThreadMember(e.ChannelID, targetUser)
				if err != nil {
					basicResp = "**Error:** Could not invite user!"
					log.Println("could not add thread member:", err)
					break
				}
			}
		case "delete":
			ch, err := s.Channel(e.ChannelID)
			if err != nil {
				basicResp = "**Error:** Could not get channel!"
				log.Println("could not get channel:", err)
				break
			}
			if ch.Type != discord.GuildPrivateThread {
				basicResp = "**Error:** This command can only be used in threads!"
				break
			}

			threadUserID, err := extractUserIDFromThreadName(ch.Name)
			if err == nil {
				addToThreadCount(e.GuildID, threadUserID, -1)
			}
			err = s.DeleteChannel(e.ChannelID, api.AuditLogReason("thread deleted by user "+e.Member.User.ID.String()+" via command"))
			if err != nil {
				basicResp = "**Error:** Could not delete thread!"
				log.Println("could not delete thread:", err)
				break
			}
			return
		case "remove":
			ch, err := s.Channel(e.ChannelID)
			if err != nil {
				basicResp = "**Error:** Could not get channel!"
				log.Println("could not get channel:", err)
				break
			}
			if ch.Type != discord.GuildPrivateThread {
				basicResp = "**Error:** This command can only be used in threads!"
				break
			}

			sn, err := data.Options.Find("user").SnowflakeValue()
			if err != nil {
				basicResp = "**Error:** User not found!"
			} else {
				targetUser := discord.UserID(sn)
				if targetUser == botID {
					basicResp = "**Error:** You cannot remove the bot!"
				} else if targetUser == e.Member.User.ID {
					basicResp = "**Error:** You cannot remove yourself!"
				} else {
					basicResp = "**Removing user!**"
					err = s.RemoveThreadMember(e.ChannelID, targetUser)
					if err != nil {
						basicResp = "**Error:** Could not remove user!"
						log.Println("could not remove thread member:", err)
						break
					}
				}
			}
		default:
			basicResp = "**Error:** Unknown command: " + data.Name
		}
	case discord.ComponentInteraction:
		err := addToThreadCount(e.GuildID, e.Member.User.ID, 1)
		if err != nil {
			basicResp = "**Error:** Each user can only have up to **" + strconv.Itoa(int(maxThreads)) + "** threads at a time!"
			break
		}
		thread, err := s.StartThreadWithoutMessage(e.ChannelID, api.StartThreadData{
			Name:                "Chat with " + e.Member.User.ID.String() + " at " + time.Now().UTC().Format(time.DateTime),
			AutoArchiveDuration: discord.OneHourArchive,
			Type:                discord.GuildPrivateThread,
			Invitable:           false,
			AuditLogReason:      "new thread",
		})
		if err != nil {
			addToThreadCount(e.GuildID, e.Member.User.ID, -1)
			basicResp = "**Error:** Could not create thread!"
			log.Println("could not create thread:", err)
			break
		}
		err = s.AddThreadMember(thread.ID, e.Member.User.ID)
		if err != nil {
			addToThreadCount(e.GuildID, e.Member.User.ID, -1)
			basicResp = "**Error:** Could not add thread member!"
			log.Println("could not add thread member:", err)
			break
		}
		resp = api.InteractionResponse{
			Type: api.MessageInteractionWithSource,
			Data: &api.InteractionResponseData{
				Content: option.NewNullableString("**Thread created!**"),
				Components: discord.ComponentsPtr(
					&discord.ButtonComponent{
						Label: "Go to thread",
						Style: discord.LinkButtonStyle("https://discord.com/channels/" + e.GuildID.String() + "/" + thread.ID.String()),
					},
				),
				Flags: discord.EphemeralMessage,
			},
		}
	default:
		// log.Printf("unknown interaction type %T", e.Data)
		return
	}

	if resp.Data == nil {
		resp = api.InteractionResponse{
			Type: api.MessageInteractionWithSource,
			Data: &api.InteractionResponseData{
				Content: option.NewNullableString(basicResp),
				Flags:   discord.EphemeralMessage,
			},
		}
	}

	if err := s.RespondInteraction(e.ID, e.Token, resp); err != nil {
		log.Println("failed to send interaction callback:", err)
	}
}

func threadMemberLeaveEvent(c *gateway.ThreadMembersUpdateEvent) {
	m, err := s.ThreadMembers(c.ID)
	if err != nil {
		log.Println("could not get thread members:", err)
		return
	}
	if len(m) == 0 || (len(m) == 1 && m[0].UserID == botID) {
		ch, err := s.Channel(c.ID)
		if err != nil {
			log.Println("could not get channel:", err)
			return
		}
		threadUserID, err := extractUserIDFromThreadName(ch.Name)
		if err == nil {
			addToThreadCount(c.GuildID, threadUserID, -1)
		}
		s.DeleteChannel(c.ID, "thread deleted due to last member leaving")
	}
}

func threadDeleteEvent(c *gateway.ThreadDeleteEvent) {
	for {
		if muti.TryLock(c.ID) {
			if threads[c.ID] != nil {
				delete(threads, c.ID)
			}
			muti.Unlock(c.ID)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func messageCreate(c *gateway.MessageCreateEvent) {
	if c.Author.ID == botID {
		return
	}

	ch, err := s.Channel(c.ChannelID)
	if err != nil {
		log.Println("could not get channel:", err)
		return
	}
	if ch.Type != discord.GuildPrivateThread {
		return
	}

	if !muti.TryLock(c.ChannelID) {
		return
	}

	if err := s.Typing(c.ChannelID); err != nil {
		log.Println("could not start typing:", err)
	}

	stoptyping := make(chan struct{})
	defer close(stoptyping)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-ticker.C:
				s.Typing(c.ChannelID)
			case <-stoptyping:
				ticker.Stop()
				muti.Unlock(c.ChannelID)
				return
			}
		}
	}()

	threads[c.ChannelID] = append(threads[c.ChannelID], ChatMessage{
		Role:    "user",
		Content: c.Content,
	})

	req := ChatRequest{
		Model:            "gpt-3.5-turbo",
		Messages:         threads[c.ChannelID],
		Temperature:      0.7,
		MaxTokens:        maxTokens,
		TopP:             1,
		FrequencyPenalty: 0,
		PresencePenalty:  0,
		User:             c.Author.ID.Mention(),
	}

	b, err := json.Marshal(req)
	if err != nil {
		log.Println("could not construct request:", err)
		s.SendMessageReply(c.ChannelID, "failed to construct request", c.ID)
		return
	}

	res, err := Post(ENDPOINT, "application/json", bytes.NewReader(b))
	if err != nil {
		log.Println("could not query openai server:", err)
		s.SendMessageReply(c.ChannelID, "failed to query openai server", c.ID)
		return
	}

	if res.StatusCode >= 400 {
		log.Println("could not query openai server:", res.Status)
		s.SendMessageReply(c.ChannelID, "failed to query openai server", c.ID)
		return
	}

	res_str, err := io.ReadAll(res.Body)
	if err != nil {
		log.Println("could not query openai server:", err)
		s.SendMessageReply(c.ChannelID, "failed to query openai server", c.ID)
		return
	}

	res.Body.Close()

	var resp ChatResponse
	err = json.Unmarshal(res_str, &resp)
	if err != nil {
		log.Println("could not decode openai response:", err)
		s.SendMessageReply(c.ChannelID, "failed to decode openai response", c.ID)
		return
	}

	for _, choice := range resp.Choices {
		threads[c.ChannelID] = append(threads[c.ChannelID], choice.Message)

		if choice.Message.Role == "assistant" {
			split := SPLIT_STR_REGEX.FindAllString(choice.Message.Content, -1)
			for i := 0; i < len(split) && i < 5; i++ {
				message := split[i]

				if i == 0 {
					if _, err := s.SendMessageReply(c.ChannelID, message, c.ID); err != nil {
						if _, err := s.SendMessage(c.ChannelID, message); err != nil {
							return
						}
					}
				} else {
					if _, err := s.SendMessage(c.ChannelID, message); err != nil {
						return
					}
				}
			}
		}
	}
}

func main() {
	if TOKEN == "" {
		log.Fatalln("missing BOT_TOKEN")
	}

	if APIKEY == "" {
		log.Fatalln("missing APIKEY")
	}

	if ENDPOINT == "" {
		ENDPOINT = "https://api.openai.com/v1/chat/completions"
	}

	if INACTIVE_TIME != "" {
		dur, err := time.ParseDuration(INACTIVE_TIME)
		if err != nil {
			log.Fatalln("invalid INACTIVE_TIME:", err)
		}
		inactiveTime = dur
	}

	if MAX_TOKENS != "" {
		tokens, err := strconv.Atoi(MAX_TOKENS)
		if err != nil || tokens < 0 {
			log.Fatalln("invalid MAX_TOKENS:", err)
		}
		maxTokens = uint(tokens)
	}

	if MAX_THREADS != "" {
		threadCount, err := strconv.Atoi(MAX_THREADS)
		if err != nil || threadCount < 0 {
			log.Fatalln("invalid MAX_THREADS:", err)
		}
		maxThreads = uint(threadCount)
	}

	s = session.New("Bot " + TOKEN)

	s.AddIntents(gateway.IntentGuilds)
	s.AddIntents(gateway.IntentGuildMessages)
	s.AddIntents(gateway.IntentGuildMembers)

	app, err := s.CurrentApplication()
	if err != nil {
		log.Fatalln("failed to get application ID:", err)
	}

	self, err := s.Me()
	if err != nil {
		log.Fatalln("identity crisis:", err)
	}

	botID = self.ID

	ctx = context.Background()

	if err := s.Open(ctx); err != nil {
		log.Fatalln("failed to connect:", err)
	}
	defer s.Close()

	newCommands := []api.CreateCommandData{
		{
			Name:                "initbuttons",
			Description:         "Create thread buttons",
			NoDefaultPermission: true,
		},
		{
			Name:        "invite",
			Description: "Invite user to thread",
			Options: []discord.CommandOption{
				&discord.UserOption{
					OptionName:  "user",
					Description: "User to invite",
					Required:    true,
				},
			},
		},
		{
			Name:        "remove",
			Description: "Remove user from thread",
			Options: []discord.CommandOption{
				&discord.UserOption{
					OptionName:  "user",
					Description: "User to remove",
					Required:    true,
				},
			},
		},
		{
			Name:        "delete",
			Description: "Delete the current thread",
		},
	}

	gs, err := s.Guilds(0)
	if err != nil {
		log.Fatalln("failed to get guilds:", err)
	}

	for _, guild := range gs {
		if _, err := s.BulkOverwriteGuildCommands(app.ID, guild.ID, newCommands); err != nil {
			log.Fatalln("failed to create guild command:", err)
		}

		activeThreads, err := s.ActiveThreads(guild.ID)
		if err != nil {
			log.Fatalln("failed to get active threads:", err)
		}

		for _, thread := range activeThreads.Threads {
			threadUserID, err := extractUserIDFromThreadName(thread.Name)
			if err == nil {
				addToThreadCount(guild.ID, threadUserID, 1)
			}

			threads[thread.ID] = []ChatMessage{}

			msgs, err := s.Messages(thread.ID, 0)
			if err != nil {
				log.Fatalln("failed to get messages:", err)
			}

			for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
				msgs[i], msgs[j] = msgs[j], msgs[i]
			}

			for _, msg := range msgs {
				if msg.Author.ID == botID {
					threads[thread.ID] = append(threads[thread.ID], ChatMessage{
						Role:    "assistant",
						Content: msg.Content,
					})
				} else {
					threads[thread.ID] = append(threads[thread.ID], ChatMessage{
						Role:    "user",
						Content: msg.Content,
					})
				}
			}
		}
	}

	s.AddHandler(messageCreate)
	s.AddHandler(threadDeleteEvent)
	s.AddHandler(interactionCreateEvent)
	s.AddHandler(threadMemberLeaveEvent)

	log.Println("Started as", self.Username)

	for {
		gs, err := s.Guilds(0)
		if err != nil {
			log.Println("failed to get guilds:", err)
			continue
		}

		for _, guild := range gs {
			activeThreads, err := s.ActiveThreads(guild.ID)
			if err != nil {
				log.Println("failed to get active threads:", err)
				continue
			}

			for _, thread := range activeThreads.Threads {
				if thread.LastMessageID.Time().Before(time.Now().Add(-inactiveTime)) {
					threadUserID, err := extractUserIDFromThreadName(thread.Name)
					if err == nil {
						addToThreadCount(guild.ID, threadUserID, -1)
					}
					s.DeleteChannel(thread.ID, "thread inactive")
				}
			}
		}
		time.Sleep(inactiveTime / 2)
	}
}
