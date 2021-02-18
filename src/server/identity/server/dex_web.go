package server

import (
	"context"
	"net/http"
	"sync"

	"github.com/pachyderm/pachyderm/v2/src/identity"

	dex_server "github.com/dexidp/dex/server"
	"github.com/jmoiron/sqlx"
	logrus "github.com/sirupsen/logrus"
)

// webDir is the path to find the static assets for the web server.
// This is always /dex-assets in the docker image, but it can be overriden for testing
var webDir = "/dex-assets"

// dexWeb wraps a Dex web server and hot reloads it when the
// issuer is reconfigured.
type dexWeb struct {
	sync.RWMutex

	// `issuer` must be a well-known URL where all pachds can reach this server.
	// Dex usually loads it from a config file, but it can be clumsy to find the
	// exact right value. Instead of requiring the user to change the config
	// we support updating it via an RPC.
	issuer       string
	server       *dex_server.Server
	serverCancel context.CancelFunc

	db              *sqlx.DB
	logger          *logrus.Entry
	storageProvider StorageProvider
}

func newDexWeb(sp StorageProvider, logger *logrus.Entry, db *sqlx.DB) *dexWeb {
	return &dexWeb{
		logger:          logger,
		storageProvider: sp,
		db:              db,
	}
}

func (w *dexWeb) updateConfig(conf identity.IdentityServerConfig) {
	w.Lock()
	defer w.Unlock()

	w.issuer = conf.Issuer
	w.stopWebServer()
	w.startWebServer()
}

// stopWebServer must be called while holding the write mutex
func (w *dexWeb) stopWebServer() {
	// Stop the background jobs for the existing server
	if w.serverCancel != nil {
		w.serverCancel()
	}

	w.server = nil
}

// startWebServer must called while holding the write mutex
func (w *dexWeb) startWebServer() {
	storage, err := w.storageProvider.GetStorage(w.logger)
	if err != nil {
		return
	}

	serverConfig := dex_server.Config{
		Storage:            storage,
		Issuer:             w.issuer,
		SkipApprovalScreen: true,
		Web: dex_server.WebConfig{
			Dir: webDir,
		},
		Logger: w.logger,
	}

	var ctx context.Context
	ctx, w.serverCancel = context.WithCancel(context.Background())
	dexServer, err := dex_server.NewServer(ctx, serverConfig)
	if err != nil {
		w.logger.WithError(err).Error("dex web server failed to start")
		return
	}

	w.server = dexServer
}

func (w *dexWeb) getServer() *dex_server.Server {
	var server *dex_server.Server
	// Get a read lock to check if the server is nil (this is rare)
	w.RLock()
	if w.server == nil {
		// If the server is nil, unlock and acquire the write lock
		w.RUnlock()
		w.Lock()
		defer w.Unlock()

		// Once we have the write lock, check that the server hasn't already been started
		if w.server == nil {
			w.startWebServer()
		}
		server = w.server
		return server
	}

	server = w.server
	w.RUnlock()
	return server
}

// interceptApproval handles the `/approval` route which is called after a user has
// authenticated to the IDP but before they're redirected back to the OIDC server
func (w *dexWeb) interceptApproval(server *dex_server.Server) func(http.ResponseWriter, *http.Request) {
	return func(rw http.ResponseWriter, r *http.Request) {
		storage, err := w.storageProvider.GetStorage(w.logger)
		if err != nil {
			return
		}
		authReq, err := storage.GetAuthRequest(r.FormValue("req"))
		if err != nil {
			w.logger.WithError(err).Error("failed to get auth request")
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !authReq.LoggedIn {
			w.logger.Error("auth request does not have an identity for approval")
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		tx, err := w.db.BeginTxx(r.Context)
		if err != nil {
			w.logger.WithError(err).Error("failed to start transaction")
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := addUserInTx(r.Context(), tx, authReq.Claims.Email); err != nil {
			w.logger.WithError(err).Error("unable to record user identity for login")
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			w.logger.WithError(err).Error("failed to commit transaction")
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		server.ServeHTTP(rw, r)
	}
}

// ServeHTTP proxies requests to the Dex server, if it's configured.
//
func (w *dexWeb) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	server := w.getServer()
	if server == nil {
		http.Error(rw, "unable to start Dex server, check logs", http.StatusInternalServerError)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/approval", w.interceptApproval(server))
	mux.HandleFunc("/", server.ServeHTTP)
	mux.ServeHTTP(rw, r)
}
