package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"maxapi/maxclient"

	"github.com/rs/zerolog/log"
)

// Account pauses. MAX answers an account that ran into one of its limits, or
// that it restricted, with an error whose code says so:
// error.user.restricted.send, too.many.requests, error.limit.violate, a limit
// on contact lookups. Carrying on after that is what turns a temporary
// restriction into a block, so the gateway stops sending from the account
// until the owner resumes it in retail. The contract is the one of the VK
// gateway's page pause (../vkapi/API.md, "Page pause and captcha"), with spam
// as the only reason:
//   - POST /chat/send/text answers the request MAX refused by its stage (503
//     lookup_failed or 422 send_rejected) plus paused, pauseReason and
//     upstreamCode. While paused it makes no MAX call and answers 422
//     send_rejected with paused and pauseReason;
//   - GET /session/status and POST /session/connect (already connected)
//     report the pause;
//   - POST /session/resume lifts it.
//
// The pause belongs to the MAX account (its MAX user id), not to a gateway
// user, like the VK gateway's page_pauses. Every gateway user logged into the
// account is paused, and the pause outlives them: a LoggedOut deletes the
// gateway user, and retail logs the owner in again through a new one, into
// the same, still restricted account. It is stored (account_pauses) before
// the answer goes out and survives a restart.

// PauseSpam is the pauseReason of every account pause.
const PauseSpam = "spam"

// pauseCodeMarkers mark a MAX error code as a limit or a restriction. They
// are matched case-insensitively anywhere in the code.
var pauseCodeMarkers = []string{"restricted", "limit", "too.many", "flood", "spam"}

// pauseWriteTimeout bounds each database call of pausing and resuming: the
// send answers within its own time budget.
const pauseWriteTimeout = 200 * time.Millisecond

// accountPause is a MAX account's pause.
type accountPause struct {
	reason       string // PauseSpam
	upstreamCode string // the MAX error code that paused the account
	since        time.Time
}

// pauseRegistry holds the pauses in memory, by MAX user id. mu also covers
// the database writes of pausing and resuming, so the stored pauses change in
// the same order as the ones in memory.
type pauseRegistry struct {
	mu     sync.Mutex
	paused map[int64]accountPause
}

func newPauseRegistry() *pauseRegistry {
	return &pauseRegistry{paused: make(map[int64]accountPause)}
}

// pauseCode returns MAX's error code when err is MAX refusing the account for
// a limit or a restriction.
func pauseCode(err error) (string, bool) {
	code, ok := maxclient.ServerErrorCode(err)
	if !ok {
		return "", false
	}
	lower := strings.ToLower(code)
	for _, marker := range pauseCodeMarkers {
		if strings.Contains(lower, marker) {
			return code, true
		}
	}
	return "", false
}

// pauseOf returns the pause of MAX account maxUserID, nil when it is not
// paused or the account is unknown (0).
func (s *server) pauseOf(maxUserID int64) *accountPause {
	if maxUserID <= 0 {
		return nil
	}
	s.pauses.mu.Lock()
	defer s.pauses.mu.Unlock()
	if p, ok := s.pauses.paused[maxUserID]; ok {
		return &p
	}
	return nil
}

// pauseOnLimit pauses MAX account maxUserID when err is MAX refusing gateway
// user userID, logged into it, for a limit or a restriction, and returns the
// pause; nil for any other error.
func (s *server) pauseOnLimit(userID string, maxUserID int64, err error) *accountPause {
	code, ok := pauseCode(err)
	if !ok {
		return nil
	}
	if maxUserID <= 0 {
		log.Error().Str("userID", userID).Str("upstreamCode", code).
			Msg("MAX refused the account for a limit or a restriction, but its MAX id is unknown: it cannot be paused")
		return nil
	}
	return s.pauseAccount(userID, maxUserID, code)
}

// pauseAccount stops sending from MAX account maxUserID, which MAX refused
// with upstreamCode on a request of gateway user userID. The pause goes into
// memory, then into the database with a bounded wait. A failed write is
// logged, and the account stays paused in memory until a restart or a
// resume. An account already paused keeps its first pause.
func (s *server) pauseAccount(userID string, maxUserID int64, upstreamCode string) *accountPause {
	s.pauses.mu.Lock()
	defer s.pauses.mu.Unlock()
	if p, ok := s.pauses.paused[maxUserID]; ok {
		log.Info().Str("userID", userID).Int64("maxUserID", maxUserID).Str("upstreamCode", upstreamCode).
			Msg("MAX refused a paused account again; it keeps its pause")
		return &p
	}

	p := accountPause{reason: PauseSpam, upstreamCode: upstreamCode, since: time.Now()}
	s.pauses.paused[maxUserID] = p
	log.Warn().Str("userID", userID).Int64("maxUserID", maxUserID).Str("upstreamCode", upstreamCode).
		Msg("MAX refused the account for a limit or a restriction; sending is paused until the owner resumes it")

	ctx, cancel := context.WithTimeout(context.Background(), pauseWriteTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO account_pauses (max_user_id, reason, upstream_code, paused_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (max_user_id) DO NOTHING`,
		maxUserID, p.reason, p.upstreamCode, p.since.Unix()); err != nil {
		log.Error().Err(err).Int64("maxUserID", maxUserID).
			Msg("Failed to store the account pause; it stays paused until a restart or a resume")
	}
	return &p
}

// resumeAccount lifts the pause of MAX account maxUserID, asked by gateway
// user userID: the stored pause first, so that a failed delete leaves it
// paused, then the one in memory. An account that is not paused, or an
// unknown one (0), stays as it is.
func (s *server) resumeAccount(userID string, maxUserID int64) error {
	s.pauses.mu.Lock()
	defer s.pauses.mu.Unlock()
	p, ok := s.pauses.paused[maxUserID]
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pauseWriteTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM account_pauses WHERE max_user_id=$1", maxUserID); err != nil {
		return err
	}
	delete(s.pauses.paused, maxUserID)
	log.Info().Str("userID", userID).Int64("maxUserID", maxUserID).Str("upstreamCode", p.upstreamCode).
		Dur("pausedFor", time.Since(p.since)).Msg("The owner resumed sending from the account")
	return nil
}

// restorePauses loads the stored pauses at startup, before any account
// connects.
func (s *server) restorePauses() error {
	var rows []struct {
		MaxUserID    int64  `db:"max_user_id"`
		Reason       string `db:"reason"`
		UpstreamCode string `db:"upstream_code"`
		PausedAt     int64  `db:"paused_at"`
	}
	if err := s.db.Select(&rows, "SELECT max_user_id, reason, upstream_code, paused_at FROM account_pauses"); err != nil {
		return err
	}
	s.pauses.mu.Lock()
	defer s.pauses.mu.Unlock()
	for _, r := range rows {
		s.pauses.paused[r.MaxUserID] = accountPause{reason: r.Reason, upstreamCode: r.UpstreamCode, since: time.Unix(r.PausedAt, 0)}
	}
	log.Info().Int("accounts", len(rows)).Msg("Restored paused MAX accounts")
	return nil
}

// addPauseFields reports the pause in a status or connect answer: paused
// always, pauseReason and upstreamCode while paused.
func addPauseFields(body map[string]interface{}, p *accountPause) {
	body["paused"] = p != nil
	if p != nil {
		body["pauseReason"] = p.reason
		body["upstreamCode"] = p.upstreamCode
	}
}
