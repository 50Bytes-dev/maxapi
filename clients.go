package main

import (
	"maxapi/maxclient"
	"sync"

	"github.com/go-resty/resty/v2"
)

// ClientManager manages MAX API clients
type ClientManager struct {
	sync.RWMutex
	maxClients   map[string]*maxclient.Client
	httpClients  map[string]*resty.Client
	myClients    map[string]*MyClient
	killChannels map[string]chan bool
}

// NewClientManager creates a new client manager
func NewClientManager() *ClientManager {
	return &ClientManager{
		maxClients:   make(map[string]*maxclient.Client),
		httpClients:  make(map[string]*resty.Client),
		myClients:    make(map[string]*MyClient),
		killChannels: make(map[string]chan bool),
	}
}

// SetMaxClient stores a MAX client for a user
func (cm *ClientManager) SetMaxClient(userID string, client *maxclient.Client) {
	cm.Lock()
	defer cm.Unlock()
	cm.maxClients[userID] = client
}

// GetMaxClient retrieves a MAX client for a user
func (cm *ClientManager) GetMaxClient(userID string) *maxclient.Client {
	cm.RLock()
	defer cm.RUnlock()
	return cm.maxClients[userID]
}

// DeleteMaxClient removes a MAX client for a user
func (cm *ClientManager) DeleteMaxClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.maxClients, userID)
}

// SetHTTPClient stores an HTTP client for a user
func (cm *ClientManager) SetHTTPClient(userID string, client *resty.Client) {
	cm.Lock()
	defer cm.Unlock()
	cm.httpClients[userID] = client
}

// GetHTTPClient retrieves an HTTP client for a user
func (cm *ClientManager) GetHTTPClient(userID string) *resty.Client {
	cm.RLock()
	defer cm.RUnlock()
	return cm.httpClients[userID]
}

// DeleteHTTPClient removes an HTTP client for a user
func (cm *ClientManager) DeleteHTTPClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.httpClients, userID)
}

// SetMyClient stores a MyClient wrapper for a user
func (cm *ClientManager) SetMyClient(userID string, client *MyClient) {
	cm.Lock()
	defer cm.Unlock()
	cm.myClients[userID] = client
}

// GetMyClient retrieves a MyClient wrapper for a user
func (cm *ClientManager) GetMyClient(userID string) *MyClient {
	cm.RLock()
	defer cm.RUnlock()
	return cm.myClients[userID]
}

// DeleteMyClient removes a MyClient wrapper for a user
func (cm *ClientManager) DeleteMyClient(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.myClients, userID)
}

// UpdateMyClientSubscriptions updates the event subscriptions of a client without reconnecting
func (cm *ClientManager) UpdateMyClientSubscriptions(userID string, subscriptions []string) {
	cm.Lock()
	defer cm.Unlock()
	if client, exists := cm.myClients[userID]; exists {
		client.subscriptions = subscriptions
	}
}

// IsConnected checks if a user has an active MAX connection
func (cm *ClientManager) IsConnected(userID string) bool {
	cm.RLock()
	defer cm.RUnlock()
	if client, exists := cm.maxClients[userID]; exists {
		return client.IsConnected()
	}
	return false
}

// CountConnected returns the number of users currently holding a live MAX
// WebSocket. Used by /health to expose liveness at a glance.
func (cm *ClientManager) CountConnected() int {
	cm.RLock()
	defer cm.RUnlock()
	n := 0
	for _, c := range cm.maxClients {
		if c != nil && c.IsConnected() {
			n++
		}
	}
	return n
}

// NewKillChannel replaces any existing kill channel for the user with a fresh
// buffered channel and returns it. Buffered so that a lone sender never blocks
// when no goroutine is currently selecting on the channel.
func (cm *ClientManager) NewKillChannel(userID string) chan bool {
	cm.Lock()
	defer cm.Unlock()
	ch := make(chan bool, 1)
	cm.killChannels[userID] = ch
	return ch
}

// GetKillChannel returns the current kill channel for the user (nil if unset).
func (cm *ClientManager) GetKillChannel(userID string) chan bool {
	cm.RLock()
	defer cm.RUnlock()
	return cm.killChannels[userID]
}

// DeleteKillChannel removes the kill channel entry for the user.
func (cm *ClientManager) DeleteKillChannel(userID string) {
	cm.Lock()
	defer cm.Unlock()
	delete(cm.killChannels, userID)
}

// SendKill delivers a non-blocking kill signal to the user's current loop
// goroutine. Returns true if the signal was enqueued, false if no channel is
// registered or the buffer is already full (meaning a kill is already pending).
func (cm *ClientManager) SendKill(userID string) bool {
	cm.RLock()
	ch := cm.killChannels[userID]
	cm.RUnlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- true:
		return true
	default:
		return false
	}
}
