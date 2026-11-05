package main

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// notifyTimeout bounds the database read of notifyAccount.
const notifyTimeout = 2 * time.Second

// notifyAccount sends postmap to every gateway user logged into MAX account
// maxUserID (by the account stored at its last login), whether its session is
// up or not, each through its own webhook and subscription. The pause and its
// end are the MAX account's, so every connection of it learns of them, also
// when another one caused them (C6).
func (s *server) notifyAccount(maxUserID int64, postmap map[string]interface{}) {
	if maxUserID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	var users []struct {
		ID    string `db:"id"`
		Token string `db:"token"`
	}
	if err := s.db.SelectContext(ctx, &users, "SELECT id, token FROM users WHERE max_user_id=$1", maxUserID); err != nil {
		log.Error().Err(err).Int64("maxUserID", maxUserID).Interface("type", postmap["type"]).
			Msg("Failed to find the gateway users of the account; the event is not sent")
		return
	}
	for _, u := range users {
		mycli := clientManager.GetMyClient(u.ID)
		if mycli == nil {
			mycli = &MyClient{userID: u.ID, token: u.Token, db: s.db, s: s}
		}
		event := make(map[string]interface{}, len(postmap))
		for k, v := range postmap {
			event[k] = v
		}
		sendEventWithWebHook(mycli, event, "")
	}
}
