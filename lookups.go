package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rs/zerolog/log"
)

// Phone lookup cache. Every send by phone used to ask MAX again
// (CONTACT_INFO_BY_PHONE, opcode 46), and repeated lookups, above all of a
// number MAX does not know, are what MAX limits and then restricts the
// account for. MAX's answer is kept per gateway user, in phone_lookups:
//   - a found contact for foundTTL. Phone numbers are reissued, so the id
//     behind one does not stay right forever; a send MAX refuses to a cached
//     id drops it too (dropLookup);
//   - MAX's explicit not.found for notFoundTTL, after which the number is
//     looked up again, as its owner may have joined MAX since. An answer
//     without a usable contact is not cached.
//
// Rows go with the gateway user. The key is the phone exactly as the lookup
// sends it to MAX. The in-memory user cache of maxclient is a separate thing
// and stays as it is.

// foundTTL is how long a found contact's MAX id is trusted.
const foundTTL = 90 * 24 * time.Hour

// notFoundTTL is how long a "MAX has no such user" answer is trusted.
const notFoundTTL = 30 * 24 * time.Hour

// lookupCacheTimeout bounds each cache read and write. A send by phone does
// a read, the lookup (4 s), a write and the send (5 s): retail gives up at
// 10 s and reads the silence as "maybe sent".
const lookupCacheTimeout = 200 * time.Millisecond

// cachedLookup returns what the cache knows of phone for gateway user
// userID. known is false when it knows nothing usable: no row, a stale
// answer, or a database error (logged), in which case the caller asks MAX.
// Otherwise contactID is the contact's MAX user id, 0 for a fresh "not
// found".
func (s *server) cachedLookup(userID, phone string) (contactID int64, known bool) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupCacheTimeout)
	defer cancel()

	var row struct {
		MaxUserID  sql.NullInt64 `db:"max_user_id"`
		LookedUpAt int64         `db:"looked_up_at"`
	}
	err := s.db.GetContext(ctx, &row,
		"SELECT max_user_id, looked_up_at FROM phone_lookups WHERE user_id=$1 AND phone=$2", userID, phone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		log.Warn().Err(err).Str("userID", userID).Msg("Failed to read the phone lookup cache; asking MAX")
		return 0, false
	}
	age := time.Since(time.Unix(row.LookedUpAt, 0))
	if row.MaxUserID.Valid {
		if age >= foundTTL {
			return 0, false
		}
		log.Info().Str("userID", userID).Str("phone", phone).Int64("contactId", row.MaxUserID.Int64).
			Msg("Phone lookup answered from the cache")
		return row.MaxUserID.Int64, true
	}
	if age < notFoundTTL {
		log.Info().Str("userID", userID).Str("phone", phone).
			Msg("Phone lookup answered from the cache: MAX has no such user")
		return 0, true
	}
	return 0, false
}

// storeLookup caches MAX's answer to the lookup of phone for gateway user
// userID: the contact's MAX user id, or 0 for MAX's not.found. A failed
// write is logged; the next send looks the number up again.
func (s *server) storeLookup(userID, phone string, contactID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupCacheTimeout)
	defer cancel()

	var maxUserID sql.NullInt64
	if contactID != 0 {
		maxUserID = sql.NullInt64{Int64: contactID, Valid: true}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO phone_lookups (user_id, phone, max_user_id, looked_up_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, phone) DO UPDATE SET max_user_id = excluded.max_user_id, looked_up_at = excluded.looked_up_at`,
		userID, phone, maxUserID, time.Now().Unix()); err != nil {
		log.Warn().Err(err).Str("userID", userID).Msg("Failed to cache the phone lookup")
	}
}

// dropLookup forgets the cached lookup of phone for gateway user userID: MAX
// refused a send to the id it held. A failed delete is logged.
func (s *server) dropLookup(userID, phone string) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupCacheTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM phone_lookups WHERE user_id=$1 AND phone=$2", userID, phone); err != nil {
		log.Warn().Err(err).Str("userID", userID).Msg("Failed to drop the cached phone lookup")
		return
	}
	log.Info().Str("userID", userID).Str("phone", phone).
		Msg("MAX refused a send to the cached contact; the number will be looked up again")
}
