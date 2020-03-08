// Copyright (c) 2019 Shivaram Lingamneni <slingamn@cs.stanford.edu>
// released under the MIT license

package irc

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zszwede/mod_irc_server/irc/history"
)

const (
	// #829, also see "Case 2" in the "three cases" below:
	zncPlaybackCommandExpiration = time.Second * 30
)

type zncCommandHandler func(client *Client, command string, params []string, rb *ResponseBuffer)

var zncHandlers = map[string]zncCommandHandler{
	"*playback": zncPlaybackHandler,
}

func zncPrivmsgHandler(client *Client, command string, privmsg string, rb *ResponseBuffer) {
	zncModuleHandler(client, command, strings.Fields(privmsg), rb)
}

func zncModuleHandler(client *Client, command string, params []string, rb *ResponseBuffer) {
	command = strings.ToLower(command)
	if subHandler, ok := zncHandlers[command]; ok {
		subHandler(client, command, params, rb)
	} else {
		nick := rb.target.Nick()
		rb.Add(nil, client.server.name, "NOTICE", nick, fmt.Sprintf(client.t("Oragono does not emulate the ZNC module %s"), command))
		rb.Add(nil, "*status!znc@znc.in", "NOTICE", nick, fmt.Sprintf(client.t("No such module [%s]"), command))
	}
}

// "number of seconds (floating point for millisecond precision) elapsed since January 1, 1970"
func zncWireTimeToTime(str string) (result time.Time) {
	var secondsPortion, fracPortion string
	dot := strings.IndexByte(str, '.')
	if dot == -1 {
		secondsPortion = str
	} else {
		secondsPortion = str[:dot]
		fracPortion = str[dot:]
	}
	seconds, _ := strconv.ParseInt(secondsPortion, 10, 64)
	fraction, _ := strconv.ParseFloat(fracPortion, 64)
	return time.Unix(seconds, int64(fraction*1000000000)).UTC()
}

type zncPlaybackTimes struct {
	start   time.Time
	end     time.Time
	targets StringSet // nil for "*" (everything), otherwise the channel names
	setAt   time.Time
}

func (z *zncPlaybackTimes) ValidFor(target string) bool {
	if z == nil {
		return false
	}

	if time.Now().Sub(z.setAt) > zncPlaybackCommandExpiration {
		return false
	}

	if z.targets == nil {
		return true
	}

	return z.targets.Has(target)
}

// https://wiki.znc.in/Playback
// PRIVMSG *playback :play <target> [lower_bound] [upper_bound]
// e.g., PRIVMSG *playback :play * 1558374442
func zncPlaybackHandler(client *Client, command string, params []string, rb *ResponseBuffer) {
	if len(params) < 2 || len(params) > 4 {
		return
	} else if strings.ToLower(params[0]) != "play" {
		return
	}
	targetString := params[1]

	now := time.Now().UTC()
	var start, end time.Time
	switch len(params) {
	case 3:
		// #831: this should have the same semantics as `LATEST timestamp=qux`,
		// or equivalently `BETWEEN timestamp=$now timestamp=qux`, as opposed to
		// `AFTER timestamp=qux` (this matters in the case where there are
		// more than znc-maxmessages available)
		start = now
		end = zncWireTimeToTime(params[2])
	case 4:
		start = zncWireTimeToTime(params[2])
		end = zncWireTimeToTime(params[3])
	}

	var targets StringSet
	var nickTargets []string

	// three cases:
	// 1. the user's PMs get played back immediately upon receiving this
	// 2. if this is a new connection (from the server's POV), save the information
	// and use it to process subsequent joins. (This is the Textual behavior:
	// first send the playback PRIVMSG, then send the JOIN lines.)
	// 3. if this is a reattach (from the server's POV), immediately play back
	// history for channels that the client is already joined to. In this scenario,
	// there are three total attempts to play the history:
	//     3.1. During the initial reattach (no-op because the *playback privmsg
	//          hasn't been received yet, but they negotiated the znc.in/playback
	//          cap so we know we're going to receive it later)
	//     3.2  Upon receiving the *playback privmsg, i.e., now: we should play
	//          the relevant history lines
	//     3.3  When the client sends a subsequent redundant JOIN line for those
	//          channels; redundant JOIN is a complete no-op so we won't replay twice

	if params[1] == "*" {
		zncPlayPrivmsgs(client, rb, "*", start, end)
	} else {
		targets = make(StringSet)
		for _, targetName := range strings.Split(targetString, ",") {
			if strings.HasPrefix(targetName, "#") {
				if cfTarget, err := CasefoldChannel(targetName); err == nil {
					targets.Add(cfTarget)
				}
			} else {
				if cfNick, err := CasefoldName(targetName); err == nil {
					nickTargets = append(nickTargets, cfNick)
				}
			}
		}
	}

	rb.session.zncPlaybackTimes = &zncPlaybackTimes{
		start:   start,
		end:     end,
		targets: targets,
		setAt:   time.Now().UTC(),
	}

	for _, channel := range client.Channels() {
		if targets == nil || targets.Has(channel.NameCasefolded()) {
			channel.autoReplayHistory(client, rb, "")
			rb.Flush(true)
		}
	}

	for _, cfNick := range nickTargets {
		zncPlayPrivmsgs(client, rb, cfNick, start, end)
		rb.Flush(true)
	}
}

func zncPlayPrivmsgs(client *Client, rb *ResponseBuffer, target string, after, before time.Time) {
	_, sequence, _ := client.server.GetHistorySequence(nil, client, target)
	if sequence == nil {
		return
	}
	zncMax := client.server.Config().History.ZNCMax
	items, _, err := sequence.Between(history.Selector{Time: after}, history.Selector{Time: before}, zncMax)
	if err == nil && len(items) != 0 {
		client.replayPrivmsgHistory(rb, items, "", true)
	}
}
