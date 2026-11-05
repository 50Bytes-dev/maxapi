package main

import (
	"sync"
	"time"

	"maxapi/maxclient"

	"github.com/rs/zerolog/log"
)

// The delivery check (C8). A send answered send_failed may have arrived. When
// it carried a ref and the gateway user is subscribed to MessageOutcome (or
// All), the gateway reads the chat's last messages a minute after the send
// and looks for the one with the send's cid, among the account's own
// messages. It sends nothing and gives out no text: one MessageOutcome tells
// whether the message is there.
//
//   - MAX unreachable (no session, no answer) gets the look made again at 3
//     and at 10 minutes after the send; then the check ends without an event.
//   - No look and no event while the account is paused, when the user is no
//     longer logged into the account that sent, when MAX refuses the look (a
//     stop signal pauses the account), or when every message read is newer
//     than the send and none is it.
//   - The pending checks live in memory: a restart drops them.

// EventMessageOutcome is the event type of the delivery check.
const EventMessageOutcome = "MessageOutcome"

// outcomeDelays are the times after the send the look is made at; tests
// shorten them.
var outcomeDelays = []time.Duration{time.Minute, 3 * time.Minute, 10 * time.Minute}

const (
	// outcomeHistoryCount is how many of the chat's last messages a look
	// reads.
	outcomeHistoryCount = 200
	// outcomeLookTimeout bounds MAX's answer to a look.
	outcomeLookTimeout = 10 * time.Second
)

// outcomeCheck is one send to confirm.
type outcomeCheck struct {
	userID    string
	token     string
	maxUserID int64
	chatID    int64
	cid       int64
	ref       string
	sentAt    time.Time
}

// outcomeChecks counts the pending checks, for tests and the log.
type outcomeChecks struct {
	mu      sync.Mutex
	pending int
	wg      sync.WaitGroup
}

func newOutcomeChecks() *outcomeChecks { return &outcomeChecks{} }

// scheduleOutcome starts the check of a send answered send_failed, when the
// user is subscribed to MessageOutcome.
func (s *server) scheduleOutcome(c outcomeCheck) {
	if !s.subscribedTo(c.userID, c.token, EventMessageOutcome) {
		return
	}
	s.outcomes.mu.Lock()
	s.outcomes.pending++
	s.outcomes.mu.Unlock()
	s.outcomes.wg.Add(1)
	time.AfterFunc(time.Until(c.sentAt.Add(outcomeDelays[0])), func() { s.runOutcome(c, 0) })
}

// runOutcome makes look number attempt (0-based) of check c.
func (s *server) runOutcome(c outcomeCheck, attempt int) {
	retry := func(why string) {
		if attempt+1 >= len(outcomeDelays) {
			log.Info().Str("userID", c.userID).Str("why", why).Msg("Delivery check ended without an outcome")
			s.endOutcome()
			return
		}
		next := attempt + 1
		time.AfterFunc(time.Until(c.sentAt.Add(outcomeDelays[next])), func() { s.runOutcome(c, next) })
	}

	if s.pauseOf(c.maxUserID) != nil {
		log.Info().Str("userID", c.userID).Msg("Delivery check dropped: the account is paused")
		s.endOutcome()
		return
	}
	client := clientManager.GetMaxClient(c.userID)
	if client == nil || !client.IsConnected() {
		retry("no session")
		return
	}
	if client.MaxUserID != c.maxUserID {
		log.Info().Str("userID", c.userID).Msg("Delivery check dropped: the user is logged into another account")
		s.endOutcome()
		return
	}
	messages, err := client.GetChatHistoryWithin(c.chatID, 0, 0, outcomeHistoryCount, outcomeLookTimeout)
	if err != nil {
		if _, refused := maxclient.ServerErrorCode(err); refused {
			s.pauseOnLimit(c.userID, c.maxUserID, err)
			log.Warn().Err(err).Str("userID", c.userID).Msg("Delivery check dropped: MAX refused the look")
			s.endOutcome()
			return
		}
		retry(err.Error())
		return
	}

	event := map[string]interface{}{
		"type":      EventMessageOutcome,
		"ref":       c.ref,
		"chatId":    c.chatID,
		"maxUserID": c.maxUserID,
	}
	olderSeen := false
	for _, m := range messages {
		if m.CID == c.cid && (m.Sender == 0 || m.Sender == c.maxUserID) {
			event["delivered"] = true
			event["messageId"] = m.ID
			s.sendOutcome(c, event)
			return
		}
		if m.Time > 0 && m.Time <= c.sentAt.UnixMilli() {
			olderSeen = true
		}
	}
	if len(messages) >= outcomeHistoryCount && !olderSeen {
		log.Info().Str("userID", c.userID).Msg("Delivery check ended without an outcome: the chat moved on too far")
		s.endOutcome()
		return
	}
	event["delivered"] = false
	s.sendOutcome(c, event)
}

func (s *server) sendOutcome(c outcomeCheck, event map[string]interface{}) {
	mycli := clientManager.GetMyClient(c.userID)
	if mycli == nil {
		mycli = &MyClient{userID: c.userID, token: c.token, db: s.db, s: s}
	}
	log.Info().Str("userID", c.userID).Interface("delivered", event["delivered"]).Msg("Delivery check found the outcome")
	sendEventWithWebHook(mycli, event, "")
	s.endOutcome()
}

func (s *server) endOutcome() {
	s.outcomes.mu.Lock()
	s.outcomes.pending--
	s.outcomes.mu.Unlock()
	s.outcomes.wg.Done()
}

// subscribedTo reports whether gateway user userID is subscribed to
// eventType, or to All.
func (s *server) subscribedTo(userID, token, eventType string) bool {
	mycli := clientManager.GetMyClient(userID)
	if mycli == nil {
		mycli = &MyClient{userID: userID, token: token, db: s.db, s: s}
	}
	subs, err := updateAndGetUserSubscriptions(mycli)
	if err != nil {
		return false
	}
	return Find(subs, eventType) || Find(subs, "All")
}
