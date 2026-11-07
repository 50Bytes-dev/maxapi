package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"maxapi/maxclient"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// A QR login (POST /session/connect without a stored login) runs as one
// goroutine per gateway user, which owns the login's WebSocket to MAX. It is
// not a session: the user has no MaxClient until MAX hands out the login
// token, so GET /session/status reports connected false meanwhile.
//
// The login ends in one of three ways:
//   - MAX hands out the token: it is stored, the session is brought up
//     (initClient), QRAuthorized and Sync go out;
//   - it fails or expires: QRExpired goes out with a machine reason, kept as
//     the user's login outcome until the next connect (GET /session/qr,
//     POST /session/password answer it);
//   - it is stopped: a new connect, POST /session/disconnect, POST
//     /session/logout or DELETE /admin/users. No event goes out, and the
//     stop waits for the goroutine, so nothing of the login reaches MAX
//     afterwards.

// Outcomes of a QR login: the reason of QRExpired, and the 410 codes of
// GET /session/qr and POST /session/password. login_expired and login_failed
// are vkapi's names.
const (
	// MAX expired the QR after maxQRRefreshes new ones, the cloud password
	// was not entered in qrPasswordTimeout, MAX refused the fifth password,
	// or no login runs and none is stored.
	LoginExpired = "login_expired"
	// MAX refused the login with an error, did not answer it, or the
	// connection was lost and could not be made again (maxQRReconnects); or
	// the gateway could not store the token.
	LoginFailed = "login_failed"
	// MAX asked for a step the gateway does not support; QRExpired names it
	// in step.
	LoginUnsupported = "login_unsupported"
)

// Codes of the session calls, besides the outcomes above.
const (
	CodeAlreadyLoggedIn     = "already_logged_in"     // 409: no login runs and a login is stored
	CodePasswordRequired    = "password_required"     // 409 on GET /session/qr: MAX asks for the cloud password
	CodePasswordNotRequired = "password_not_required" // 409 on POST /session/password: a login runs, MAX has not asked for it
	CodeInvalidPassword     = "invalid_password"      // 422: MAX refused the password
	CodeLoginUnavailable    = "login_unavailable"     // 503 on POST /session/connect: MAX refused to issue a QR
)

// Bounds of a QR login.
var (
	// maxQRRefreshes caps how many new QRs a login gets when MAX expires
	// one; the next expiry ends it with login_expired.
	maxQRRefreshes = 10
	// maxQRReconnects caps how many times a login connects to MAX again
	// after its WebSocket dropped; one more drop ends it with login_failed.
	maxQRReconnects = 5
	// qrReconnectDelay is the wait before the first reconnect of a login;
	// it doubles per attempt, up to qrReconnectCap.
	qrReconnectDelay = time.Second
	qrReconnectCap   = 15 * time.Second
	// qrPollDefault is the poll interval when MAX names none (or less than
	// qrPollFloor).
	qrPollDefault = 5 * time.Second
	qrPollFloor   = time.Second
	// qrPollSilence ends a login with login_failed when MAX has not
	// answered its poll for that long.
	qrPollSilence = 5 * time.Minute
	// qrPasswordTimeout ends a login waiting for the cloud password with
	// login_expired.
	qrPasswordTimeout = 10 * time.Minute
	// maxPasswordAttempts is how many passwords MAX may refuse; the last
	// refusal ends the login with login_expired.
	maxPasswordAttempts = 5
	// qrStopWait bounds how long a stop waits for the login goroutine.
	qrStopWait = 10 * time.Second
)

// maxWSURI is the MAX WebSocket endpoint sessions and logins dial; empty
// means maxclient's default. Tests point it at a fake server.
var maxWSURI = ""

// newMaxClient returns a MAX client of the user for deviceID.
func newMaxClient(userID, deviceID string) *maxclient.Client {
	client := maxclient.NewClient(deviceID, log.With().Str("userID", userID).Logger())
	client.URI = maxWSURI
	return client
}

// qrPhase is where a QR login stands.
type qrPhase int

const (
	qrPhaseScan     qrPhase = iota // a QR is shown; the owner has not confirmed it yet
	qrPhasePassword                // MAX asks for the cloud password (QRPasswordRequired)
	qrPhaseDone                    // MAX handed out the token; the session is being brought up
)

// passwordAttempt is a password POST /session/password hands to the login.
type passwordAttempt struct {
	password string
	reply    chan apiError
}

// apiError is an error answer of a session call; status 200 means success.
// upstream is MAX's error code, when MAX answered with one.
type apiError struct {
	status   int
	code     string
	msg      string
	upstream string
}

// respondAPIError answers e as an error body, with upstreamCode when MAX
// answered with an error.
func (s *server) respondAPIError(w http.ResponseWriter, r *http.Request, e apiError) {
	body := map[string]interface{}{"success": false, "error": e.msg, "code": e.code}
	if e.upstream != "" {
		body["upstreamCode"] = e.upstream
	}
	s.Respond(w, r, e.status, body)
}

// qrLogin is one QR login of a gateway user.
type qrLogin struct {
	s        *server
	userID   string
	token    string // the gateway user's API token
	deviceID string
	mycli    *MyClient // for the login's webhook events only; not registered

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	passwords chan passwordAttempt

	mu        sync.Mutex
	client    *maxclient.Client
	phase     qrPhase
	qrcode    string // data URI of the current QR
	scanned   bool   // the current QR was scanned
	trackID   string // of the current QR
	challenge *maxclient.PasswordChallenge
}

// qrLoginRegistry holds the running QR login of each user, and the outcome
// of the last one that ended, until the next connect.
type qrLoginRegistry struct {
	mu       sync.Mutex
	logins   map[string]*qrLogin
	outcomes map[string]string
}

var qrLogins = &qrLoginRegistry{logins: map[string]*qrLogin{}, outcomes: map[string]string{}}

// running returns the user's running login, nil when none runs.
func (r *qrLoginRegistry) running(userID string) *qrLogin {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.logins[userID]
}

// outcome returns how the user's last login ended, "" when none is kept.
func (r *qrLoginRegistry) outcome(userID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcomes[userID]
}

// replace registers l as the user's login, forgets the last outcome, and
// returns the login it replaced.
func (r *qrLoginRegistry) replace(l *qrLogin) *qrLogin {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.logins[l.userID]
	r.logins[l.userID] = l
	delete(r.outcomes, l.userID)
	return old
}

// end unregisters l, when it is still the user's login, and keeps outcome
// unless it is "".
func (r *qrLoginRegistry) end(l *qrLogin, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logins[l.userID] != l {
		return
	}
	delete(r.logins, l.userID)
	if outcome != "" {
		r.outcomes[l.userID] = outcome
	}
}

// stop stops the user's running login without an event, waits for its
// goroutine (at most qrStopWait), and forgets the last outcome.
func (r *qrLoginRegistry) stop(userID string) {
	r.mu.Lock()
	l := r.logins[userID]
	delete(r.logins, userID)
	delete(r.outcomes, userID)
	r.mu.Unlock()
	if l != nil {
		l.stopAndWait()
	}
}

// stopAndWait cancels the login and waits for its goroutine to end.
func (l *qrLogin) stopAndWait() {
	l.cancel()
	select {
	case <-l.done:
	case <-time.After(qrStopWait):
		log.Warn().Str("userID", l.userID).Msg("The QR login did not stop in time")
	}
}

// startQRLogin starts a QR login for the user, stopping the one running.
// It returns once MAX issued the first QR, so GET /session/qr has it at
// once; QRGenerated goes out with it. An error is an apiError.
func (s *server) startQRLogin(userID, token string) error {
	var deviceID string
	_ = s.db.Get(&deviceID, "SELECT COALESCE(device_id,'') FROM users WHERE id=$1", userID)
	if deviceID == "" {
		deviceID = uuid.New().String()
	}

	ctx, cancel := context.WithCancel(context.Background())
	l := &qrLogin{
		s:         s,
		userID:    userID,
		token:     token,
		deviceID:  deviceID,
		mycli:     &MyClient{userID: userID, token: token, db: s.db, s: s},
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		passwords: make(chan passwordAttempt),
	}
	if old := qrLogins.replace(l); old != nil {
		old.stopAndWait()
	}

	client, result, err := l.open(true)
	if err != nil {
		qrLogins.end(l, "")
		cancel()
		close(l.done)
		var upstream string
		if code, ok := maxclient.ServerErrorCode(err); ok {
			upstream = code
		}
		log.Warn().Err(err).Str("userID", userID).Str("upstreamCode", upstream).Msg("Could not start the QR login")
		if upstream != "" {
			return apiError{http.StatusServiceUnavailable, CodeLoginUnavailable, "MAX refused to issue a QR", upstream}
		}
		return apiError{status: http.StatusServiceUnavailable, code: CodeNotConnected, msg: "could not reach MAX to issue a QR"}
	}

	if _, err := s.db.Exec("UPDATE users SET device_id=$1 WHERE id=$2", deviceID, userID); err != nil {
		log.Error().Err(err).Msg("Failed to persist device_id")
	}
	l.setClient(client)
	l.showQR(result)
	go l.run(result)
	return nil
}

func (e apiError) Error() string { return e.msg }

// open connects a new WebSocket of the login and, with withQR, asks MAX for
// a QR. The client is shut down when the login is stopped.
func (l *qrLogin) open(withQR bool) (*maxclient.Client, *maxclient.QRStartResult, error) {
	client := newMaxClient(l.userID, l.deviceID)
	stop := context.AfterFunc(l.ctx, client.Shutdown)
	fail := func(err error) (*maxclient.Client, *maxclient.QRStartResult, error) {
		stop()
		client.Shutdown()
		return nil, nil, err
	}
	if err := client.Connect(); err != nil {
		return fail(err)
	}
	if err := client.SessionInit(nil); err != nil {
		return fail(err)
	}
	var result *maxclient.QRStartResult
	if withQR {
		var err error
		if result, err = client.RequestQRCode(); err != nil {
			return fail(err)
		}
	}
	client.StartPingLoop()
	return client, result, nil
}

// setClient makes client the login's WebSocket, shutting the previous one.
func (l *qrLogin) setClient(client *maxclient.Client) {
	l.mu.Lock()
	old := l.client
	l.client = client
	l.mu.Unlock()
	if old != nil && old != client {
		old.Shutdown()
	}
}

// currentClient returns the login's WebSocket.
func (l *qrLogin) currentClient() *maxclient.Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.client
}

// showQR stores a new QR of the login and sends QRGenerated.
func (l *qrLogin) showQR(result *maxclient.QRStartResult) {
	qrcode, err := l.s.renderAndStoreQR(l.userID, result)
	if err != nil {
		log.Error().Err(err).Str("userID", l.userID).Msg("Could not render the QR")
	}
	l.mu.Lock()
	l.qrcode = qrcode
	l.scanned = false
	l.trackID = result.TrackID
	l.mu.Unlock()
	l.event(map[string]interface{}{
		"type":         maxclient.EventTypeQRGenerated,
		"qrLink":       result.QRLink,
		"qrCodeBase64": qrcode,
		"trackId":      result.TrackID,
		"expiresAt":    result.ExpiresAt,
	})
}

// event sends a webhook event of the login, unless it was stopped.
func (l *qrLogin) event(postmap map[string]interface{}) {
	if l.ctx.Err() != nil {
		return
	}
	sendEventWithWebHook(l.mycli, postmap, "")
}

// stopped reports whether the login was stopped.
func (l *qrLogin) stopped() bool { return l.ctx.Err() != nil }

// sleep waits d, false when the login was stopped first.
func (l *qrLogin) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-l.ctx.Done():
		return false
	}
}

// run is the login's goroutine: it polls the QR, refreshes it, reconnects,
// confirms, waits for the cloud password, and ends the login.
func (l *qrLogin) run(first *maxclient.QRStartResult) {
	defer close(l.done)
	outcome, detail := l.loop(first)

	if client := l.currentClient(); client != nil {
		client.Shutdown()
	}
	if l.stopped() {
		qrLogins.end(l, "")
		return
	}
	qrLogins.end(l, outcome)
	if outcome == "" {
		return
	}
	l.s.clearQRState(l.userID)
	payload := map[string]interface{}{"type": maxclient.EventTypeQRExpired, "reason": outcome}
	for k, v := range detail {
		payload[k] = v
	}
	log.Info().Str("userID", l.userID).Str("reason", outcome).Msg("The QR login ended without a login")
	l.event(payload)
}

// qrDeadline is when the QR of result is taken for expired: MAX's TTL plus
// a margin, at least a minute.
func qrDeadline(result *maxclient.QRStartResult) time.Time {
	ttl := time.Duration(result.TTL) * time.Millisecond
	if ttl < time.Minute {
		ttl = time.Minute
	}
	return time.Now().Add(ttl + 15*time.Second)
}

// pollInterval is MAX's poll interval of result, bounded by qrPollFloor.
func pollInterval(result *maxclient.QRStartResult) time.Duration {
	d := time.Duration(result.PollingInterval) * time.Millisecond
	if d < qrPollFloor {
		d = qrPollDefault
	}
	return d
}

// loop runs the login until it ends: "" when MAX handed out the token or the
// login was stopped, else the outcome and the fields QRExpired adds.
func (l *qrLogin) loop(first *maxclient.QRStartResult) (string, map[string]interface{}) {
	result := first
	deadline := qrDeadline(result)
	refreshes := 0
	reconnects := 0
	lastAnswer := time.Now()

	// newQR asks MAX for a new QR on the current WebSocket, false when the
	// budget is used up or MAX refused (outcome set).
	newQR := func() (ok bool, outcome string) {
		if refreshes >= maxQRRefreshes {
			return false, LoginExpired
		}
		refreshes++
		next, err := l.currentClient().RequestQRCode()
		if err != nil {
			log.Warn().Err(err).Str("userID", l.userID).Msg("QR refresh failed")
			if _, fromMAX := maxclient.ServerErrorCode(err); fromMAX {
				return false, LoginFailed
			}
			return false, ""
		}
		result = next
		deadline = qrDeadline(result)
		lastAnswer = time.Now()
		l.showQR(result)
		return true, ""
	}

	// reconnect connects the login again after its WebSocket dropped and
	// issues a new QR on it: the old one belongs to the dead connection.
	reconnect := func() bool {
		for {
			if reconnects >= maxQRReconnects {
				log.Warn().Str("userID", l.userID).Int("reconnects", reconnects).Msg("The QR login lost its connection too many times")
				return false
			}
			delay := qrReconnectDelay << uint(reconnects)
			if delay <= 0 || delay > qrReconnectCap {
				delay = qrReconnectCap
			}
			reconnects++
			log.Warn().Str("userID", l.userID).Int("attempt", reconnects).Dur("in", delay).Msg("The QR login lost its connection; connecting again")
			if !l.sleep(delay) {
				return false
			}
			// Waiting for the cloud password, the login goes on by the
			// challenge's trackId: no new QR.
			password := l.phaseIs(qrPhasePassword)
			client, next, err := l.open(!password)
			if err != nil {
				log.Warn().Err(err).Str("userID", l.userID).Msg("The QR login could not connect again")
				continue
			}
			l.setClient(client)
			if password {
				return true
			}
			refreshes++
			result = next
			deadline = qrDeadline(result)
			lastAnswer = time.Now()
			l.showQR(result)
			return true
		}
	}

	for {
		if l.stopped() {
			return "", nil
		}
		if time.Now().After(deadline) {
			if ok, outcome := newQR(); !ok {
				if outcome == "" && !l.currentClient().IsConnected() {
					if reconnect() {
						continue
					}
					return l.lost()
				}
				if outcome == "" {
					outcome = LoginFailed
				}
				return outcome, nil
			}
			continue
		}
		if !l.sleep(pollInterval(result)) {
			return "", nil
		}

		client := l.currentClient()
		status, err := client.PollQRStatus(result.TrackID)
		if err != nil {
			if l.stopped() {
				return "", nil
			}
			if !client.IsConnected() {
				if reconnect() {
					continue
				}
				return l.lost()
			}
			if code, fromMAX := maxclient.ServerErrorCode(err); fromMAX {
				log.Warn().Str("userID", l.userID).Str("upstreamCode", code).Msg("MAX refused the QR poll")
				return LoginFailed, nil
			}
			log.Warn().Err(err).Str("userID", l.userID).Msg("QR poll failed")
			if time.Since(lastAnswer) > qrPollSilence {
				return LoginFailed, nil
			}
			continue
		}
		lastAnswer = time.Now()

		switch status {
		case maxclient.QRStatusPending:
			continue
		case maxclient.QRStatusExpired:
			if ok, outcome := newQR(); !ok {
				if outcome == "" {
					outcome = LoginFailed
				}
				return outcome, nil
			}
			continue
		case maxclient.QRStatusScanned:
			if l.markScanned() {
				l.event(map[string]interface{}{
					"type":    maxclient.EventTypeQRScanned,
					"trackId": result.TrackID,
				})
			}
			step, err := client.ConfirmQRLogin(result.TrackID)
			if err != nil {
				if l.stopped() {
					return "", nil
				}
				if !client.IsConnected() {
					// The confirmation was lost with the connection: a new
					// QR on a new one.
					if reconnect() {
						continue
					}
					return l.lost()
				}
				log.Warn().Err(err).Str("userID", l.userID).Msg("QR confirm failed")
				return LoginFailed, nil
			}
			return l.next(step, reconnect)
		}
	}
}

// lost is the outcome of a login whose connection could not be made again.
func (l *qrLogin) lost() (string, map[string]interface{}) {
	if l.stopped() {
		return "", nil
	}
	return LoginFailed, nil
}

// next acts on what MAX answered a confirmation or a password with.
func (l *qrLogin) next(step *maxclient.LoginStep, reconnect func() bool) (string, map[string]interface{}) {
	switch {
	case step.Token != "":
		return l.authorize(step.Token)
	case step.Password != nil:
		return l.awaitPassword(step.Password, reconnect)
	default:
		log.Warn().Str("userID", l.userID).Str("step", step.Unsupported).Msg("MAX asked for a login step the gateway does not support")
		return LoginUnsupported, map[string]interface{}{"step": step.Unsupported}
	}
}

func (l *qrLogin) phaseIs(p qrPhase) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.phase == p
}

// markScanned marks the current QR scanned, true the first time.
func (l *qrLogin) markScanned() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.scanned {
		return false
	}
	l.scanned = true
	return true
}

// awaitPassword waits for the cloud password MAX asks for, sends each one
// POST /session/password hands over, and ends the login with the token, or
// when the time runs out or MAX refuses maxPasswordAttempts passwords.
func (l *qrLogin) awaitPassword(ch *maxclient.PasswordChallenge, reconnect func() bool) (string, map[string]interface{}) {
	l.mu.Lock()
	l.phase = qrPhasePassword
	l.challenge = ch
	l.mu.Unlock()
	l.s.clearQRState(l.userID)

	event := map[string]interface{}{"type": maxclient.EventTypeQRPasswordRequired}
	if ch.Hint != "" {
		event["hint"] = ch.Hint
	}
	if ch.Email != "" {
		event["email"] = ch.Email
	}
	log.Info().Str("userID", l.userID).Msg("MAX asks for the cloud password")
	l.event(event)

	timeout := time.NewTimer(qrPasswordTimeout)
	defer timeout.Stop()
	watch := time.NewTicker(qrPollFloor)
	defer watch.Stop()
	refused := 0

	for {
		select {
		case <-l.ctx.Done():
			return "", nil
		case <-timeout.C:
			return LoginExpired, nil
		case <-watch.C:
			// Keep the WebSocket up while the owner types.
			if !l.currentClient().IsConnected() {
				if !reconnect() {
					return l.lost()
				}
			}
		case a := <-l.passwords:
			client := l.currentClient()
			step, err := client.SubmitPassword(ch.TrackID, a.password)
			if err != nil {
				code, fromMAX := maxclient.ServerErrorCode(err)
				if !fromMAX {
					a.reply <- apiError{status: http.StatusServiceUnavailable, code: CodeNotConnected, msg: "MAX did not answer; send the password again"}
					if !client.IsConnected() && !reconnect() {
						return l.lost()
					}
					continue
				}
				refusal := maxclient.PasswordRefusal(code)
				log.Info().Str("userID", l.userID).Str("upstreamCode", code).Str("refusal", refusal).Msg("MAX refused the cloud password")
				switch refusal {
				case maxclient.PasswordWrong:
					refused++
					if refused >= maxPasswordAttempts {
						a.reply <- apiError{http.StatusGone, LoginExpired, "MAX refused the password too many times; the login has ended", code}
						return LoginExpired, nil
					}
					a.reply <- apiError{http.StatusUnprocessableEntity, CodeInvalidPassword, "MAX refused the password", code}
					continue
				case maxclient.PasswordExpired:
					a.reply <- apiError{http.StatusGone, LoginExpired, "MAX ended the password step; the login has ended", code}
					return LoginExpired, nil
				case maxclient.PasswordUnsupported:
					a.reply <- apiError{http.StatusGone, LoginUnsupported, "MAX will not take the password from the gateway", code}
					return LoginUnsupported, map[string]interface{}{"step": "password:" + code}
				default:
					a.reply <- apiError{http.StatusGone, LoginFailed, "MAX refused the login", code}
					return LoginFailed, nil
				}
			}
			switch {
			case step.Token != "":
				a.reply <- apiError{status: http.StatusOK}
				return l.authorize(step.Token)
			case step.Password != nil:
				// MAX asks again: the password was not taken.
				refused++
				if refused >= maxPasswordAttempts {
					a.reply <- apiError{status: http.StatusGone, code: LoginExpired, msg: "MAX refused the password too many times; the login has ended"}
					return LoginExpired, nil
				}
				ch = step.Password
				l.mu.Lock()
				l.challenge = ch
				l.mu.Unlock()
				a.reply <- apiError{status: http.StatusUnprocessableEntity, code: CodeInvalidPassword, msg: "MAX refused the password"}
			default:
				a.reply <- apiError{status: http.StatusGone, code: LoginUnsupported, msg: "MAX asked for a login step the gateway does not support"}
				log.Warn().Str("userID", l.userID).Str("step", step.Unsupported).Msg("MAX asked for a login step the gateway does not support")
				return LoginUnsupported, map[string]interface{}{"step": step.Unsupported}
			}
		}
	}
}

// submitPassword hands a cloud password to the user's login and waits for
// MAX's answer to it.
func (l *qrLogin) submitPassword(password string) apiError {
	reply := make(chan apiError, 1)
	select {
	case l.passwords <- passwordAttempt{password: password, reply: reply}:
	case <-l.done:
		return l.endedError()
	case <-time.After(5 * time.Second):
		return apiError{status: http.StatusServiceUnavailable, code: CodeNotConnected, msg: "the login is busy; send the password again"}
	}
	select {
	case r := <-reply:
		return r
	case <-l.done:
		select {
		case r := <-reply:
			return r
		default:
		}
		return l.endedError()
	}
}

// endedError answers a call to a login that has ended meanwhile.
func (l *qrLogin) endedError() apiError {
	if outcome := qrLogins.outcome(l.userID); outcome != "" {
		return outcomeError(outcome)
	}
	return apiError{status: http.StatusGone, code: LoginExpired, msg: "no QR login runs"}
}

// outcomeError is the 410 answer of a login that ended with outcome.
func outcomeError(outcome string) apiError {
	switch outcome {
	case LoginFailed:
		return apiError{status: http.StatusGone, code: LoginFailed, msg: "the QR login failed; connect again"}
	case LoginUnsupported:
		return apiError{status: http.StatusGone, code: LoginUnsupported, msg: "MAX asked for a login step the gateway does not support"}
	}
	return apiError{status: http.StatusGone, code: LoginExpired, msg: "the QR login expired; connect again"}
}

// authorize stores the token MAX handed out, closes the login's WebSocket
// and brings the session up (completeQRAuth), which sends QRAuthorized and
// Sync.
func (l *qrLogin) authorize(authToken string) (string, map[string]interface{}) {
	l.mu.Lock()
	l.phase = qrPhaseDone
	l.challenge = nil
	l.mu.Unlock()

	if l.stopped() {
		return "", nil
	}
	if _, err := l.s.db.Exec(
		`UPDATE users SET auth_token=$1, qr_track_id='', qr_code_base64='', qr_link='', qr_expires_at=0 WHERE id=$2`,
		authToken, l.userID,
	); err != nil {
		log.Error().Err(err).Str("userID", l.userID).Msg("Failed to persist auth token")
		return LoginFailed, nil
	}
	// The login's WebSocket goes before the session's opens.
	if client := l.currentClient(); client != nil {
		client.Shutdown()
	}
	l.s.completeQRAuth(l.ctx, l.userID, authToken)
	return "", nil
}

// ========== SESSION CALLS ==========

// GetQR returns the QR of the user's running login. Errors: 409
// password_required, 409 already_logged_in, 410 with the outcome of the last
// login (login_expired when there is none).
func (s *server) GetQR() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		if l := qrLogins.running(txtid); l != nil {
			l.mu.Lock()
			phase, qrcode, scanned := l.phase, l.qrcode, l.scanned
			l.mu.Unlock()
			switch phase {
			case qrPhaseScan:
				s.Respond(w, r, http.StatusOK, map[string]interface{}{
					"success": true,
					"qrcode":  qrcode,
					"scanned": scanned,
				})
				return
			case qrPhasePassword:
				s.respondError(w, r, http.StatusConflict, CodePasswordRequired,
					"MAX asks for the account's cloud password: send it with POST /session/password")
				return
			}
		}
		if s.loginStored(txtid) {
			s.respondError(w, r, http.StatusConflict, CodeAlreadyLoggedIn, "already logged in")
			return
		}
		e := outcomeError(qrLogins.outcome(txtid))
		if qrLogins.outcome(txtid) == "" {
			e.msg = "no QR login runs; start one with POST /session/connect"
		}
		s.respondError(w, r, e.status, e.code, e.msg)
	}
}

// maxPasswordLength bounds the cloud password the gateway passes on.
const maxPasswordLength = 256

// SubmitPassword passes the account's cloud password to the user's QR login
// when MAX asks for it (QRPasswordRequired). 200 means MAX took it:
// QRAuthorized and Sync follow.
func (s *server) SubmitPassword() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		txtid := r.Context().Value("userinfo").(Values).Get("Id")

		var body PasswordBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, "could not decode payload")
			return
		}
		if body.Password == "" || len(body.Password) > maxPasswordLength || strings.ContainsAny(body.Password, "\x00") {
			s.respondError(w, r, http.StatusBadRequest, ErrInvalidInput, fmt.Sprintf("password must be 1 to %d bytes", maxPasswordLength))
			return
		}

		l := qrLogins.running(txtid)
		if l == nil || l.phaseIs(qrPhaseDone) {
			if s.loginStored(txtid) {
				s.respondError(w, r, http.StatusConflict, CodeAlreadyLoggedIn, "already logged in")
				return
			}
			e := outcomeError(qrLogins.outcome(txtid))
			s.respondAPIError(w, r, e)
			return
		}
		if !l.phaseIs(qrPhasePassword) {
			s.respondError(w, r, http.StatusConflict, CodePasswordNotRequired, "MAX has not asked for a password")
			return
		}
		res := l.submitPassword(body.Password)
		if res.status != http.StatusOK {
			s.respondAPIError(w, r, res)
			return
		}
		s.Respond(w, r, http.StatusOK, map[string]interface{}{"success": true, "accepted": true})
	}
}

// loginStored reports whether the user has a stored MAX login.
func (s *server) loginStored(userID string) bool {
	var authToken string
	_ = s.db.Get(&authToken, "SELECT COALESCE(auth_token,'') FROM users WHERE id=$1", userID)
	return authToken != ""
}

// errorAs is errors.As for apiError.
func asAPIError(err error) (apiError, bool) {
	var e apiError
	if errors.As(err, &e) {
		return e, true
	}
	return apiError{}, false
}
