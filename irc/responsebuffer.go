// Copyright (c) 2016-2017 Daniel Oaks <daniel@danieloaks.net>
// released under the MIT license

package irc

import (
	"runtime/debug"
	"time"

	"github.com/goshuirc/irc-go/ircmsg"
	"github.com/zszwede/mod_irc_server/irc/caps"
	"github.com/zszwede/mod_irc_server/irc/utils"
)

const (
	// https://ircv3.net/specs/extensions/labeled-response.html
	defaultBatchType = "labeled-response"
)

// ResponseBuffer - put simply - buffers messages and then outputs them to a given client.
//
// Using a ResponseBuffer lets you really easily implement labeled-response, since the
// buffer will silently create a batch if required and label the outgoing messages as
// necessary (or leave it off and simply tag the outgoing message).
type ResponseBuffer struct {
	Label     string // label if this is a labeled response batch
	batchID   string // ID of the labeled response batch, if one has been initiated
	batchType string // type of the labeled response batch (possibly `history` or `chathistory`)

	// stack of batch IDs of nested batches, which are handled separately
	// from the underlying labeled-response batch. starting a new nested batch
	// unconditionally enqueues its batch start message; subsequent messages
	// are tagged with the nested batch ID, until nested batch end.
	// (the nested batch start itself may have no batch tag, or the batch tag of the
	// underlying labeled-response batch, or the batch tag of the next outermost
	// nested batch.)
	nestedBatches []string

	messages  []ircmsg.IrcMessage
	finalized bool
	target    *Client
	session   *Session
}

// GetLabel returns the label from the given message.
func GetLabel(msg ircmsg.IrcMessage) string {
	_, value := msg.GetTag(caps.LabelTagName)
	return value
}

// NewResponseBuffer returns a new ResponseBuffer.
func NewResponseBuffer(session *Session) *ResponseBuffer {
	return &ResponseBuffer{
		session:   session,
		target:    session.client,
		batchType: defaultBatchType,
	}
}

func (rb *ResponseBuffer) AddMessage(msg ircmsg.IrcMessage) {
	if rb.finalized {
		rb.target.server.logger.Error("internal", "message added to finalized ResponseBuffer, undefined behavior")
		debug.PrintStack()
		// TODO(dan): send a NOTICE to the end user with a string representation of the message,
		// for debugging purposes
		return
	}

	rb.session.setTimeTag(&msg, time.Time{})
	rb.setNestedBatchTag(&msg)

	rb.messages = append(rb.messages, msg)
}

func (rb *ResponseBuffer) setNestedBatchTag(msg *ircmsg.IrcMessage) {
	if 0 < len(rb.nestedBatches) {
		msg.SetTag("batch", rb.nestedBatches[len(rb.nestedBatches)-1])
	}
}

// Add adds a standard new message to our queue.
func (rb *ResponseBuffer) Add(tags map[string]string, prefix string, command string, params ...string) {
	rb.AddMessage(ircmsg.MakeMessage(tags, prefix, command, params...))
}

// Broadcast adds a standard new message to our queue, then sends an unlabeled copy
// to all other sessions.
func (rb *ResponseBuffer) Broadcast(tags map[string]string, prefix string, command string, params ...string) {
	// can't reuse the IrcMessage object because of tag pollution :-\
	rb.Add(tags, prefix, command, params...)
	for _, session := range rb.session.client.Sessions() {
		if session != rb.session {
			session.Send(tags, prefix, command, params...)
		}
	}
}

// AddFromClient adds a new message from a specific client to our queue.
func (rb *ResponseBuffer) AddFromClient(time time.Time, msgid string, fromNickMask string, fromAccount string, tags map[string]string, command string, params ...string) {
	msg := ircmsg.MakeMessage(nil, fromNickMask, command, params...)
	if rb.session.capabilities.Has(caps.MessageTags) {
		msg.UpdateTags(tags)
	}

	// attach account-tag
	if rb.session.capabilities.Has(caps.AccountTag) && fromAccount != "*" {
		msg.SetTag("account", fromAccount)
	}
	// attach message-id
	if len(msgid) > 0 && rb.session.capabilities.Has(caps.MessageTags) {
		msg.SetTag("msgid", msgid)
	}
	// attach server-time
	rb.session.setTimeTag(&msg, time)

	rb.AddMessage(msg)
}

// AddSplitMessageFromClient adds a new split message from a specific client to our queue.
func (rb *ResponseBuffer) AddSplitMessageFromClient(fromNickMask string, fromAccount string, tags map[string]string, command string, target string, message utils.SplitMessage) {
	if message.Is512() {
		rb.AddFromClient(message.Time, message.Msgid, fromNickMask, fromAccount, tags, command, target, message.Message)
	} else {
		if rb.session.capabilities.Has(caps.Multiline) {
			batch := rb.session.composeMultilineBatch(fromNickMask, fromAccount, tags, command, target, message)
			rb.setNestedBatchTag(&batch[0])
			rb.setNestedBatchTag(&batch[len(batch)-1])
			rb.messages = append(rb.messages, batch...)
		} else {
			for i, messagePair := range message.Split {
				var msgid string
				if i == 0 {
					msgid = message.Msgid
				}
				rb.AddFromClient(message.Time, msgid, fromNickMask, fromAccount, tags, command, target, messagePair.Message)
			}
		}
	}
}

func (rb *ResponseBuffer) sendBatchStart(blocking bool) {
	if rb.batchID != "" {
		// batch already initialized
		return
	}

	rb.batchID = rb.session.generateBatchID()
	message := ircmsg.MakeMessage(nil, rb.target.server.name, "BATCH", "+"+rb.batchID, rb.batchType)
	if rb.Label != "" {
		message.SetTag(caps.LabelTagName, rb.Label)
	}
	rb.session.SendRawMessage(message, blocking)
}

func (rb *ResponseBuffer) sendBatchEnd(blocking bool) {
	if rb.batchID == "" {
		// we are not sending a batch, skip this
		return
	}

	message := ircmsg.MakeMessage(nil, rb.target.server.name, "BATCH", "-"+rb.batchID)
	rb.session.SendRawMessage(message, blocking)
}

// Starts a nested batch (see the ResponseBuffer struct definition for a description of
// how this works)
func (rb *ResponseBuffer) StartNestedBatch(batchType string, params ...string) (batchID string) {
	batchID = rb.session.generateBatchID()
	msgParams := make([]string, len(params)+2)
	msgParams[0] = "+" + batchID
	msgParams[1] = batchType
	copy(msgParams[2:], params)
	rb.AddMessage(ircmsg.MakeMessage(nil, rb.target.server.name, "BATCH", msgParams...))
	rb.nestedBatches = append(rb.nestedBatches, batchID)
	return
}

// Ends a nested batch
func (rb *ResponseBuffer) EndNestedBatch(batchID string) {
	if batchID == "" {
		return
	}

	if 0 == len(rb.nestedBatches) || rb.nestedBatches[len(rb.nestedBatches)-1] != batchID {
		rb.target.server.logger.Error("internal", "inconsistent batch nesting detected")
		debug.PrintStack()
		return
	}

	rb.nestedBatches = rb.nestedBatches[0 : len(rb.nestedBatches)-1]
	rb.AddMessage(ircmsg.MakeMessage(nil, rb.target.server.name, "BATCH", "-"+batchID))
}

// Convenience to start a nested batch for history lines, at the highest level
// supported by the client (`history`, `chathistory`, or no batch, in descending order).
func (rb *ResponseBuffer) StartNestedHistoryBatch(params ...string) (batchID string) {
	var batchType string
	if rb.session.capabilities.Has(caps.EventPlayback) {
		batchType = "history"
	} else if rb.session.capabilities.Has(caps.Batch) {
		batchType = "chathistory"
	}
	if batchType != "" {
		batchID = rb.StartNestedBatch(batchType, params...)
	}
	return
}

// Send sends all messages in the buffer to the client.
// Afterwards, the buffer is in an undefined state and MUST NOT be used further.
// If `blocking` is true you MUST be sending to the client from its own goroutine.
func (rb *ResponseBuffer) Send(blocking bool) error {
	return rb.flushInternal(true, blocking)
}

// Flush sends all messages in the buffer to the client.
// Afterwards, the buffer can still be used. Client code MUST subsequently call Send()
// to ensure that the final `BATCH -` message is sent.
// If `blocking` is true you MUST be sending to the client from its own goroutine.
func (rb *ResponseBuffer) Flush(blocking bool) error {
	return rb.flushInternal(false, blocking)
}

// detects whether the response buffer consists of a single, unflushed nested batch,
// in which case it can be collapsed down to that batch
func (rb *ResponseBuffer) isCollapsible() (result bool) {
	// rb.batchID indicates that we already flushed some lines
	if rb.batchID != "" || len(rb.messages) < 2 {
		return false
	}
	first, last := rb.messages[0], rb.messages[len(rb.messages)-1]
	if first.Command != "BATCH" || last.Command != "BATCH" {
		return false
	}
	if len(first.Params) == 0 || len(first.Params[0]) == 0 || len(last.Params) == 0 || len(last.Params[0]) == 0 {
		return false
	}
	return first.Params[0][1:] == last.Params[0][1:]
}

// flushInternal sends the contents of the buffer, either blocking or nonblocking
// It sends the `BATCH +` message if the client supports it and it hasn't been sent already.
// If `final` is true, it also sends `BATCH -` (if necessary).
func (rb *ResponseBuffer) flushInternal(final bool, blocking bool) error {
	if rb.finalized {
		return nil
	}

	if rb.session.capabilities.Has(caps.LabeledResponse) && rb.Label != "" {
		if final && rb.isCollapsible() {
			// collapse to the outermost nested batch
			rb.messages[0].SetTag(caps.LabelTagName, rb.Label)
		} else if !final || 2 <= len(rb.messages) {
			// we either have 2+ messages, or we are doing a Flush() and have to assume
			// there will be more messages in the future
			rb.sendBatchStart(blocking)
		} else if len(rb.messages) == 1 && rb.batchID == "" {
			// single labeled message
			rb.messages[0].SetTag(caps.LabelTagName, rb.Label)
		} else if len(rb.messages) == 0 && rb.batchID == "" {
			// ACK message
			message := ircmsg.MakeMessage(nil, rb.session.client.server.name, "ACK")
			message.SetTag(caps.LabelTagName, rb.Label)
			rb.session.setTimeTag(&message, time.Time{})
			rb.session.SendRawMessage(message, blocking)
		}
	}

	// send each message out
	for _, message := range rb.messages {
		// attach batch ID, unless this message was part of a nested batch and is
		// already tagged
		if rb.batchID != "" && !message.HasTag("batch") {
			message.SetTag("batch", rb.batchID)
		}

		// send message out
		rb.session.SendRawMessage(message, blocking)
	}

	// end batch if required
	if final {
		rb.sendBatchEnd(blocking)
		rb.finalized = true
	}

	// clear out any existing messages
	rb.messages = rb.messages[:0]

	return nil
}

// Notice sends the client the given notice from the server.
func (rb *ResponseBuffer) Notice(text string) {
	rb.Add(nil, rb.target.server.name, "NOTICE", rb.target.Nick(), text)
}
