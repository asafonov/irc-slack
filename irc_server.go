package main

import (
	"fmt"
	"html"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nlopes/slack"
)

// Project constants
const (
	ProjectAuthor       = "Andrea Barberio"
	ProjectAuthorEmail  = "insomniac@slackware.it"
	ProjectURL          = "https://github.com/insomniacslk/irc-slack"
	MaxSlackAPIAttempts = 3
)

// IrcCommandHandler is the prototype that every IRC command handler has to implement
type IrcCommandHandler func(*IrcContext, string, string, []string, string)

// IrcCommandHandlers maps each IRC command to its handler function
var IrcCommandHandlers = map[string]IrcCommandHandler{
	"CAP":     IrcCapHandler,
	"NICK":    IrcNickHandler,
	"USER":    IrcUserHandler,
	"PING":    IrcPingHandler,
	"PRIVMSG": IrcPrivMsgHandler,
	"QUIT":    IrcQuitHandler,
	"MODE":    IrcModeHandler,
	"PASS":    IrcPassHandler,
	"WHOIS":   IrcWhoisHandler,
	"JOIN":    IrcJoinHandler,
	"PART":    IrcPartHandler,
}

var (
	rxSlackUrls = regexp.MustCompile("<[^>]+>?")
	rxSlackUser = regexp.MustCompile("<@[UW][A-Z0-9]+>")
)

// ExpandText expands and unquotes text and URLs from Slack's messages. Slack
// quotes the text and URLS, and the latter are enclosed in < and >. It also
// translates potential URLs into actual URLs (e.g. when you type "example.com"),
// so you will get something like <http://example.com|example.com>. This
// function tries to detect them and unquote and expand them for a better
// visualization on IRC.
func ExpandText(text string) string {
	text = rxSlackUrls.ReplaceAllStringFunc(text, func(subs string) string {
		if !strings.HasPrefix(subs, "<") && !strings.HasSuffix(subs, ">") {
			return subs
		}

		// Slack URLs may contain an URL followed by a "|", followed by the
		// original message. Detect the pipe and only parse the URL.
		var (
			slackURL = subs[1 : len(subs)-1]
			slackMsg string
		)
		idx := strings.LastIndex(slackURL, "|")
		if idx >= 0 {
			slackMsg = slackURL[idx+1:]
			slackURL = slackURL[:idx]
		}

		u, err := url.Parse(slackURL)
		if err != nil {
			return subs
		}
		// Slack escapes the URLs passed by the users, let's undo that
		//u.RawQuery = html.UnescapeString(u.RawQuery)
		if slackMsg == "" {
			return u.String()
		}
		return fmt.Sprintf("%s (%s)", slackMsg, u.String())
	})
	text = html.UnescapeString(text)
	return text
}

// SendIrcNumeric sends a numeric code message to the recipient
func SendIrcNumeric(ctx *IrcContext, code int, args, desc string) error {
	reply := fmt.Sprintf(":%s %03d %s :%s\r\n", ctx.ServerName, code, args, desc)
	log.Printf("Sending numeric reply: %s", reply)
	_, err := ctx.Conn.Write([]byte(reply))
	return err
}

// IrcSendChanInfoAfterJoin sends channel information to the user about a joined
// channel.
func IrcSendChanInfoAfterJoin(ctx *IrcContext, name, topic string, members []string, isGroup bool) {
	// TODO wrap all these Conn.Write into a function
	ctx.Conn.Write([]byte(fmt.Sprintf(":%v JOIN #%v\r\n", ctx.Mask(), name)))
	// RPL_TOPIC
	SendIrcNumeric(ctx, 332, fmt.Sprintf("%s #%s", ctx.Nick, name), topic)
	// RPL_NAMREPLY
	SendIrcNumeric(ctx, 353, fmt.Sprintf("%s = #%s", ctx.Nick, name), strings.Join(ctx.UserIDsToNames(members...), " "))
	// RPL_ENDOFNAMES
	SendIrcNumeric(ctx, 366, fmt.Sprintf("%s #%s", ctx.Nick, name), "End of NAMES list")
	ctx.ChanMutex.Lock()
	ctx.Channels[name] = Channel{Topic: topic, Members: members, IsGroup: isGroup}
	ctx.ChanMutex.Unlock()
}

func usersInConversation(ctx *IrcContext, conversation string) ([]string, error) {
	var (
		members, m []string
		nextCursor string
		err        error
		page       int
	)
	for {
		attempt := 0
		for {
			// retry if rate-limited, no more than MaxSlackAPIAttempts times
			if attempt >= MaxSlackAPIAttempts {
				return nil, fmt.Errorf("GetUsersInConversation: exceeded the maximum number of attempts (%d) with the Slack API", MaxSlackAPIAttempts)
			}
			log.Printf("GetUsersInConversation: page %d attempt #%d nextCursor=%s", page, attempt, nextCursor)
			m, nextCursor, err = ctx.SlackClient.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: conversation, Cursor: nextCursor})
			if err != nil {
				log.Printf("Err: %v", err)
				if rlErr, ok := err.(*slack.RateLimitedError); ok {
					// we were rate-limited. Let's wait as much as Slack
					// instructs us to do
					log.Printf("Hit Slack API rate limiter. Waiting %v", rlErr.RetryAfter)
					time.Sleep(rlErr.RetryAfter)
					attempt++
					continue
				}
				return nil, fmt.Errorf("Cannot get member list for conversation %s: %v", conversation, err)
			}
			break
		}
		members = append(members, m...)
		if nextCursor == "" {
			break
		}
		page++
	}
	return members, nil
}

// join will join the channel with the given ID, name and topic, and send back a
// response to the IRC client
func join(ctx *IrcContext, id, name, topic string) error {
	members, err := usersInConversation(ctx, id)
	if err != nil {
		return err
	}
	info := "(joined) "
	info += fmt.Sprintf(" topic=%s members=%d", topic, len(members))
	log.Printf(info)
	// the channels are already joined, notify the IRC client of their
	// existence
	go IrcSendChanInfoAfterJoin(ctx, name, topic, members, false)
	return nil
}

// joinChannels gets all the available Slack channels and sends an IRC JOIN message
// for each of the joined channels on Slack
func joinChannels(ctx *IrcContext) error {
	log.Print("Channel list:")
	var (
		channels, chans []slack.Channel
		nextCursor      string
		err             error
	)
	for {
		attempt := 0
		for {
			// retry if rate-limited, no more than MaxSlackAPIAttempts times
			if attempt >= MaxSlackAPIAttempts {
				return fmt.Errorf("GetConversations: exceeded the maximum number of attempts (%d) with the Slack API", MaxSlackAPIAttempts)
			}
			log.Printf("GetConversations: attempt #%d, nextCursor=%s", attempt, nextCursor)
			chans, nextCursor, err = ctx.SlackClient.GetConversations(&slack.GetConversationsParameters{Cursor: nextCursor})
			if err != nil {
				log.Printf("Err: %v", err)
				if rlErr, ok := err.(*slack.RateLimitedError); ok {
					// we were rate-limited. Let's wait as much as Slack
					// instructs us to do
					log.Printf("Hit Slack API rate limiter. Waiting %v", rlErr.RetryAfter)
					time.Sleep(rlErr.RetryAfter)
					attempt++
					continue
				}
				return fmt.Errorf("Cannot get slack channels: %v", err)
			}
			break
		}
		channels = append(channels, chans...)
		if nextCursor == "" {
			break
		}
	}
	for _, ch := range channels {
		if ch.IsMember {
			if err := join(ctx, ch.ID, ch.Name, ch.Topic.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// IrcAfterLoggingIn is called once the user has successfully logged on IRC
func IrcAfterLoggingIn(ctx *IrcContext, rtm *slack.RTM) error {
	// Send a welcome to the user, to let the client knows that it's connected
	// RPL_WELCOME
	SendIrcNumeric(ctx, 1, ctx.Nick, fmt.Sprintf("Welcome to the %s IRC chat, %s!", ctx.ServerName, ctx.Nick))
	// RPL_MOTDSTART
	SendIrcNumeric(ctx, 375, ctx.Nick, "")
	// RPL_MOTD
	SendIrcNumeric(ctx, 372, ctx.Nick, fmt.Sprintf("This is an IRC-to-Slack gateway, written by %s <%s>.", ProjectAuthor, ProjectAuthorEmail))
	SendIrcNumeric(ctx, 372, ctx.Nick, fmt.Sprintf("More information at %s.", ProjectURL))
	SendIrcNumeric(ctx, 372, ctx.Nick, fmt.Sprintf("Slack team name: %s", ctx.SlackRTM.GetInfo().Team.Name))
	// RPL_ENDOFMOTD
	SendIrcNumeric(ctx, 376, ctx.Nick, "")

	ctx.Channels = make(map[string]Channel)
	ctx.ChanMutex = &sync.Mutex{}

	// get channels
	if err := joinChannels(ctx); err != nil {
		return err
	}

	go eventHandler(ctx, rtm)
	return nil
}

// IrcCapHandler is called when a CAP command is sent
func IrcCapHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) > 1 {
		if args[0] == "LS" {
			reply := fmt.Sprintf(":%s CAP * LS :\r\n", ctx.ServerName)
			ctx.Conn.Write([]byte(reply))
		} else {
			log.Printf("Got CAP %v", args)
		}
	}
}

// IrcPrivMsgHandler is called when a PRIVMSG command is sent
func IrcPrivMsgHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) != 1 {
		log.Printf("Invalid PRIVMSG command args: %v", args)
	}
	target := args[0]
	if !strings.HasPrefix(target, "#") {
		// Send to user instead of channel
		target = "@" + target
	}
	text := trailing
	params := slack.NewPostMessageParameters()
	params.AsUser = true
	ctx.SlackClient.PostMessage(target, text, params)
}

func connectToSlack(ctx *IrcContext) error {
	ctx.SlackClient = slack.New(ctx.SlackAPIKey)
	logger := log.New(os.Stdout, "slack: ", log.Lshortfile|log.LstdFlags)
	slack.SetLogger(logger)
	ctx.SlackClient.SetDebug(false)
	rtm := ctx.SlackClient.NewRTM()
	ctx.SlackRTM = rtm
	go rtm.ManageConnection()
	log.Print("Starting Slack client")
	// Wait until the websocket is connected, then print client info
	var info *slack.Info
	// FIXME tune the timeout to a value that makes sense
	timeout := 10 * time.Second
	start := time.Now()
	for {
		if info = rtm.GetInfo(); info != nil {
			break
		}
		if time.Now().After(start.Add(timeout)) {
			return fmt.Errorf("Connection to Slack timed out after %v", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Print("CLIENT INFO:")
	log.Printf("  URL     : %v", info.URL)
	log.Printf("  User    : %v", *info.User)
	log.Printf("  Team    : %v", *info.Team)
	log.Printf("  Users   : %v", info.Users)
	log.Printf("  Channels: %v", info.Channels)
	log.Printf("  Groups  : %v", info.Groups)
	log.Printf("  Bots    : %v", info.Bots)
	log.Printf("  IMs     : %v", info.IMs)
	ctx.Nick = info.User.Name
	return IrcAfterLoggingIn(ctx, rtm)
}

// IrcNickHandler is called when a NICK command is sent
func IrcNickHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) < 1 {
		log.Printf("Invalid NICK command args: %v", args)
	}
	// The nickname cannot be changed here. Just set it to whatever Slack says
	// you are.
	if ctx.SlackClient == nil {
		if err := connectToSlack(ctx); err != nil {
			log.Printf("Error: cannot connect to Slack: %v", err)
			// close the IRC connection to the client
			ctx.Conn.Close()
		}
	}
	// ctx.SlackRTM.GetInfo() should not be `nil` at this points. If it is, it's ok
	// to panic here
	ctx.Nick = ctx.SlackRTM.GetInfo().User.Name
	if args[0] != ctx.Nick {
		// the client is trying to use a different nickname, let's tell them
		// they can't.
		// RPL_SAVENICK
		SendIrcNumeric(
			ctx, 43, args[0],
			fmt.Sprintf("Your nickname is %s and cannot be changed", ctx.Nick),
		)
	}
	log.Printf("Setting nickname for %v to %v", ctx.Conn.RemoteAddr(), ctx.Nick)
}

// IrcUserHandler is called when a USER command is sent
func IrcUserHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if ctx.Nick == "" {
		log.Print("Empty nickname!")
		return
	}
	if len(args) < 3 {
		log.Printf("Invalid USER command args: %s", args)
	}
	log.Printf("Contexts: %v", UserContexts)
	// TODO implement `mode` as per https://tools.ietf.org/html/rfc2812#section-3.1.3
	username, _, _ := args[0], args[1], args[2]
	ctx.UserName = username
	ctx.RealName = trailing
}

// IrcPingHandler is called when a PING command is sent
func IrcPingHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	msg := fmt.Sprintf("PONG %s", strings.Join(args, " "))
	if trailing != "" {
		msg += " :" + trailing
	}
	ctx.Conn.Write([]byte(msg + "\r\n"))
}

// IrcQuitHandler is called when a QUIT command is sent
func IrcQuitHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	ctx.Conn.Close()
}

// IrcModeHandler is called when a MODE command is sent
func IrcModeHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) == 1 {
		// get mode request. Always no mode (for now)
		mode := "+"
		// RPL_CHANNELMODEIS
		SendIrcNumeric(ctx, 324, fmt.Sprintf("%s %s %s", ctx.Nick, args[0], mode), "")
	} else if len(args) > 1 {
		// set mode request. Not handled yet
		// TODO handle mode set
		// ERR_UMODEUNKNOWNFLAG
		SendIrcNumeric(ctx, 501, args[0], fmt.Sprintf("Unknown MODE flags %s", strings.Join(args[1:], " ")))
	} else {
		// TODO send an error
	}
}

// IrcPassHandler is called when a PASS command is sent
func IrcPassHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) != 1 {
		log.Printf("Invalid PASS arguments: %s", args)
		// ERR_PASSWDMISMATCH
		SendIrcNumeric(ctx, 464, "", "Invalid password")
		return
	}
	ctx.SlackAPIKey = args[0]
}

// IrcWhoisHandler is called when a WHOIS command is sent
func IrcWhoisHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) != 1 && len(args) != 2 {
		// ERR_UNKNOWNERROR
		SendIrcNumeric(ctx, 400, ctx.Nick, "Invalid WHOIS command")
		return
	}
	username := args[0]
	// if the second argument is the same as the first, it's a request of WHOIS
	// with idle time
	// TODO handle idle time, args[1]
	user := ctx.GetUserInfoByName(username)
	if user == nil {
		// ERR_NOSUCHNICK
		SendIrcNumeric(ctx, 401, ctx.Nick, fmt.Sprintf("No such nick %s", username))
	} else {
		// RPL_WHOISUSER
		SendIrcNumeric(ctx, 311, fmt.Sprintf("%s %s %s %s *", username, user.Name, user.ID, "localhost"), user.RealName)
		// RPL_WHOISSERVER
		SendIrcNumeric(ctx, 312, fmt.Sprintf("%s %s", username, ctx.ServerName), ctx.ServerName)
		// RPL_ENDOFWHOIS
		SendIrcNumeric(ctx, 319, ctx.Nick, username)
	}
}

// IrcJoinHandler is called when a JOIN command is sent
func IrcJoinHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) != 1 {
		// ERR_UNKNOWNERROR
		SendIrcNumeric(ctx, 400, ctx.Nick, "Invalid JOIN command")
		return
	}
	channame := args[0]
	ch, err := ctx.SlackClient.JoinChannel(channame)
	if err != nil {
		log.Printf("Cannot join channel %s: %v", channame, err)
		return
	}
	log.Printf("Joined channel %s", ch.Name)
	go IrcSendChanInfoAfterJoin(ctx, ch.Name, ch.Topic.Value, ch.Members, true)
}

// IrcPartHandler is called when a JOIN command is sent
func IrcPartHandler(ctx *IrcContext, prefix, cmd string, args []string, trailing string) {
	if len(args) != 1 {
		// ERR_UNKNOWNERROR
		SendIrcNumeric(ctx, 400, ctx.Nick, "Invalid PART command")
		return
	}
	channame := args[0]
	if strings.HasPrefix(channame, "#") {
		channame = channame[1:]
	}
	// Slack needs the channel ID to leave it, not the channel name. The only
	// way to get the channel ID from the name is retrieving the whole channel
	// list and finding the one whose name is the one we want to leave
	chanlist, err := ctx.SlackClient.GetChannels(true)
	if err != nil {
		log.Printf("Cannot leave channel %s: %v", channame, err)
		// ERR_UNKNOWNERROR
		SendIrcNumeric(ctx, 400, ctx.Nick, fmt.Sprintf("Cannot leave channel: %v", err))
		return
	}
	var chanID string
	for _, ch := range chanlist {
		if ch.Name == channame {
			chanID = ch.ID
			log.Printf("Trying to leave channel: %+v", ch)
			break
		}
	}
	if chanID == "" {
		// ERR_USERNOTINCHANNEL
		SendIrcNumeric(ctx, 441, ctx.Nick, fmt.Sprintf("User is not in channel %s", channame))
		return
	}
	notInChan, err := ctx.SlackClient.LeaveChannel(chanID)
	if err != nil {
		log.Printf("Cannot leave channel %s (id: %s): %v", channame, chanID, err)
		return
	}
	if notInChan {
		// ERR_USERNOTINCHANNEL
		SendIrcNumeric(ctx, 441, ctx.Nick, fmt.Sprintf("User is not in channel %s", channame))
		return
	}
	log.Printf("Left channel %s", channame)
	ctx.Conn.Write([]byte(fmt.Sprintf(":%v PART #%v\r\n", ctx.Mask(), channame)))
}
