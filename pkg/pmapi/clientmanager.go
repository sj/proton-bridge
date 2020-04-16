package pmapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ClientManager is a manager of clients.
type ClientManager struct {
	// newClient is used to create new Clients. By default this creates pmapi clients but it can be overridden to
	// create other types of clients (e.g. for integration tests).
	newClient func(userID string) Client

	config       *ClientConfig
	roundTripper http.RoundTripper

	clients       map[string]Client
	clientsLocker sync.Locker

	tokens       map[string]string
	tokensLocker sync.Locker

	expirations       map[string]*tokenExpiration
	expirationsLocker sync.Locker

	bridgeAuths chan ClientAuth
	clientAuths chan ClientAuth

	host, scheme string
	hostLocker   sync.RWMutex

	allowProxy       bool
	proxyProvider    *proxyProvider
	proxyUseDuration time.Duration

	idGen idGen

	log *logrus.Entry
}

type idGen int

func (i *idGen) next() int {
	(*i)++
	return int(*i)
}

// ClientAuth holds an API auth produced by a Client for a specific user.
type ClientAuth struct {
	UserID string
	Auth   *Auth
}

// tokenExpiration manages the expiration of an access token.
type tokenExpiration struct {
	timer  *time.Timer
	cancel chan (struct{})
}

// NewClientManager creates a new ClientMan which manages clients configured with the given client config.
func NewClientManager(config *ClientConfig) (cm *ClientManager) {
	cm = &ClientManager{
		config:       config,
		roundTripper: http.DefaultTransport,

		clients:       make(map[string]Client),
		clientsLocker: &sync.Mutex{},

		tokens:       make(map[string]string),
		tokensLocker: &sync.Mutex{},

		expirations:       make(map[string]*tokenExpiration),
		expirationsLocker: &sync.Mutex{},

		host:       RootURL,
		scheme:     rootScheme,
		hostLocker: sync.RWMutex{},

		bridgeAuths: make(chan ClientAuth),
		clientAuths: make(chan ClientAuth),

		proxyProvider:    newProxyProvider(dohProviders, proxyQuery),
		proxyUseDuration: proxyUseDuration,

		log: logrus.WithField("pkg", "pmapi-manager"),
	}

	cm.newClient = func(userID string) Client {
		return newClient(cm, userID)
	}

	go cm.forwardClientAuths()

	return cm
}

func (cm *ClientManager) SetClientConstructor(f func(userID string) Client) {
	cm.newClient = f
}

// SetRoundTripper sets the roundtripper used by clients created by this client manager.
func (cm *ClientManager) SetRoundTripper(rt http.RoundTripper) {
	cm.roundTripper = rt
}

// GetClient returns a client for the given userID.
// If the client does not exist already, it is created.
func (cm *ClientManager) GetClient(userID string) Client {
	cm.clientsLocker.Lock()
	defer cm.clientsLocker.Unlock()

	if client, ok := cm.clients[userID]; ok {
		return client
	}

	cm.clients[userID] = cm.newClient(userID)

	return cm.clients[userID]
}

// GetAnonymousClient returns an anonymous client. It replaces any anonymous client that was already created.
func (cm *ClientManager) GetAnonymousClient() Client {
	return cm.GetClient(fmt.Sprintf("anonymous-%v", cm.idGen.next()))
}

// LogoutClient logs out the client with the given userID and ensures its sensitive data is successfully cleared.
func (cm *ClientManager) LogoutClient(userID string) {
	client, ok := cm.clients[userID]

	if !ok {
		return
	}

	delete(cm.clients, userID)

	go func() {
		if !strings.HasPrefix(userID, "anonymous-") {
			for client.DeleteAuth() == ErrAPINotReachable {
				cm.log.Warn("Logging out client failed because API was not reachable, retrying...")
			}
		}
		client.ClearData()
		cm.clearToken(userID)
	}()
}

// GetRootURL returns the full root URL (scheme+host).
func (cm *ClientManager) GetRootURL() string {
	cm.hostLocker.RLock()
	defer cm.hostLocker.RUnlock()

	return fmt.Sprintf("%v://%v", cm.scheme, cm.host)
}

// getHost returns the host to make requests to.
// It does not include the protocol i.e. no "https://" (use getScheme for that).
func (cm *ClientManager) getHost() string {
	cm.hostLocker.RLock()
	defer cm.hostLocker.RUnlock()

	return cm.host
}

// IsProxyAllowed returns whether the user has allowed us to switch to a proxy if need be.
func (cm *ClientManager) IsProxyAllowed() bool {
	cm.hostLocker.RLock()
	defer cm.hostLocker.RUnlock()

	return cm.allowProxy
}

// AllowProxy allows the client manager to switch clients over to a proxy if need be.
func (cm *ClientManager) AllowProxy() {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	cm.allowProxy = true
}

// DisallowProxy prevents the client manager from switching clients over to a proxy if need be.
func (cm *ClientManager) DisallowProxy() {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	cm.allowProxy = false
	cm.host = RootURL
}

// IsProxyEnabled returns whether we are currently proxying requests.
func (cm *ClientManager) IsProxyEnabled() bool {
	cm.hostLocker.RLock()
	defer cm.hostLocker.RUnlock()

	return cm.host != RootURL
}

// switchToReachableServer switches to using a reachable server (either proxy or standard API).
func (cm *ClientManager) switchToReachableServer() (proxy string, err error) {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	logrus.Info("Attempting to switch to a proxy")

	if proxy, err = cm.proxyProvider.findReachableServer(); err != nil {
		err = errors.Wrap(err, "failed to find a usable proxy")
		return
	}

	logrus.WithField("proxy", proxy).Info("Switching to a proxy")

	// If the host is currently the RootURL, it's the first time we are enabling a proxy.
	// This means we want to disable it again in 24 hours.
	if cm.host == RootURL {
		go func() {
			<-time.After(cm.proxyUseDuration)
			cm.host = RootURL
		}()
	}

	cm.host = proxy

	return
}

// GetToken returns the token for the given userID.
func (cm *ClientManager) GetToken(userID string) string {
	cm.tokensLocker.Lock()
	defer cm.tokensLocker.Unlock()

	return cm.tokens[userID]
}

// GetAuthUpdateChannel returns a channel on which client auths can be received.
func (cm *ClientManager) GetAuthUpdateChannel() chan ClientAuth {
	return cm.bridgeAuths
}

// GetClientAuthChannel returns a channel on which clients should send auths.
func (cm *ClientManager) GetClientAuthChannel() chan ClientAuth {
	return cm.clientAuths
}

// forwardClientAuths handles all incoming auths from clients before forwarding them on the bridge auth channel.
func (cm *ClientManager) forwardClientAuths() {
	for auth := range cm.clientAuths {
		logrus.Debug("ClientManager received auth from client")
		cm.handleClientAuth(auth)
		logrus.Debug("ClientManager is forwarding auth to bridge")
		cm.bridgeAuths <- auth
	}
}

// SetTokenIfUnset sets the token for the given userID if it wasn't already set.
// The token does not expire.
func (cm *ClientManager) SetTokenIfUnset(userID, token string) {
	cm.tokensLocker.Lock()
	defer cm.tokensLocker.Unlock()

	if _, ok := cm.tokens[userID]; ok {
		return
	}

	logrus.WithField("userID", userID).Info("Setting token because it is currently unset")

	cm.tokens[userID] = token
}

// setToken sets the token for the given userID with the given expiration time.
func (cm *ClientManager) setToken(userID, token string, expiration time.Duration) {
	cm.tokensLocker.Lock()
	defer cm.tokensLocker.Unlock()

	logrus.WithField("userID", userID).Info("Updating token")

	cm.tokens[userID] = token

	cm.setTokenExpiration(userID, expiration)

	// TODO: This should be one go routine per all tokens.
	go cm.watchTokenExpiration(userID)
}

// setTokenExpiration will ensure the token is refreshed if it expires.
// If the token already has an expiration time set, it is replaced.
func (cm *ClientManager) setTokenExpiration(userID string, expiration time.Duration) {
	cm.expirationsLocker.Lock()
	defer cm.expirationsLocker.Unlock()

	if exp, ok := cm.expirations[userID]; ok {
		exp.timer.Stop()
		close(exp.cancel)
	}

	cm.expirations[userID] = &tokenExpiration{
		timer:  time.NewTimer(expiration),
		cancel: make(chan struct{}),
	}
}

func (cm *ClientManager) clearToken(userID string) {
	cm.tokensLocker.Lock()
	defer cm.tokensLocker.Unlock()

	logrus.WithField("userID", userID).Info("Clearing token")

	delete(cm.tokens, userID)
}

// handleClientAuth updates or clears client authorisation based on auths received.
func (cm *ClientManager) handleClientAuth(ca ClientAuth) {
	cm.clientsLocker.Lock()
	defer cm.clientsLocker.Unlock()

	// If we aren't managing this client, there's nothing to do.
	if _, ok := cm.clients[ca.UserID]; !ok {
		logrus.WithField("userID", ca.UserID).Info("Not handling auth for unmanaged client")
		return
	}

	// If the auth is nil, we should clear the token.
	// TODO: Maybe we should trigger a client logout here? Then we don't have to remember to log it out ourself.
	if ca.Auth == nil {
		cm.clearToken(ca.UserID)
		return
	}

	cm.setToken(ca.UserID, ca.Auth.GenToken(), time.Duration(ca.Auth.ExpiresIn)*time.Second)
}

func (cm *ClientManager) watchTokenExpiration(userID string) {
	cm.expirationsLocker.Lock()
	expiration := cm.expirations[userID]
	cm.expirationsLocker.Unlock()

	select {
	case <-expiration.timer.C:
		cm.log.WithField("userID", userID).Info("Auth token expired! Refreshing")
		if _, err := cm.clients[userID].AuthRefresh(cm.tokens[userID]); err != nil {
			cm.log.WithField("userID", userID).
				WithError(err).
				Error("Token refresh failed before expiration")
		}

	case <-expiration.cancel:
		logrus.WithField("userID", userID).Debug("Auth was refreshed before it expired")
	}
}