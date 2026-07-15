package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/chromedp/chromedp"
	"github.com/gorilla/mux"
	"github.com/mitchellh/go-homedir"
	"github.com/peterbourgon/ff/v3/ffcli"
	"go.etcd.io/bbolt"
	"go.uber.org/zap"

	"github.com/watchers-id/watchersid/pkg/api"
	"github.com/watchers-id/watchersid/pkg/chrome"
	"github.com/watchers-id/watchersid/pkg/db/bolt"
	"github.com/watchers-id/watchersid/pkg/proj"
	"github.com/watchers-id/watchersid/pkg/proxy"
	"github.com/watchers-id/watchersid/pkg/proxy/intercept"
	"github.com/watchers-id/watchersid/pkg/reqlog"
	"github.com/watchers-id/watchersid/pkg/scope"
	"github.com/watchers-id/watchersid/pkg/sender"
)

var version = "0.0.0"

//go:embed admin
//go:embed admin/_next/static
//go:embed admin/_next/static/chunks/pages/*.js
//go:embed admin/_next/static/*/*.js
var adminContent embed.FS

var watchersidUsage = `
Usage:
    watchersid [flags] [subcommand] [flags]

Runs an HTTP server with (MITM) proxy, GraphQL service, and a web based admin interface.

Options:
    --cert         Path to root CA certificate. Creates file if it doesn't exist. (Default: "~/.watchersid/watchersid_cert.pem")
    --key          Path to root CA private key. Creates file if it doesn't exist. (Default: "~/.watchersid/watchersid_key.pem")
    --db           Database file path. Creates file if it doesn't exist. (Default: "~/.watchersid/watchersid.db")
    --addr         TCP address for HTTP server to listen on, in the form \"host:port\". (Default: "127.0.0.1:8080")
    --token        Admin UI/API access token. Auto-generated and persisted if left empty.
    --no-auth      Disable the admin token check. Not recommended.
    --chrome       Launch Chrome with proxy settings applied and certificate errors ignored. (Default: false)
    --verbose      Enable verbose logging.
    --json         Encode logs as JSON, instead of pretty/human readable output.
    --version, -v  Output version.
    --help, -h     Output this usage text.

Subcommands:
    - cert  Certificate management

Run ` + "`watchersid <subcommand> --help`" + ` for subcommand specific usage instructions.
`

type WatchersIDCommand struct {
	config *Config

	cert    string
	key     string
	db      string
	addr    string
	token   string
	noAuth  bool
	chrome  bool
	version bool
}

func NewWatchersIDCommand() (*ffcli.Command, *Config) {
	cmd := WatchersIDCommand{
		config: &Config{},
	}

	fs := flag.NewFlagSet("watchersid", flag.ExitOnError)

	fs.StringVar(&cmd.cert, "cert", "~/.watchersid/watchersid_cert.pem",
		"Path to root CA certificate. Creates a new certificate if file doesn't exist.")
	fs.StringVar(&cmd.key, "key", "~/.watchersid/watchersid_key.pem",
		"Path to root CA private key. Creates a new private key if file doesn't exist.")
	fs.StringVar(&cmd.db, "db", "~/.watchersid/watchersid.db", "Database file path. Creates file if it doesn't exist.")
	fs.StringVar(&cmd.addr, "addr", "127.0.0.1:8080", "TCP address to listen on, in the form \"host:port\". "+
		"Defaults to localhost-only; use 0.0.0.0:PORT to allow LAN/remote access (make sure --token/auth stays enabled).")
	fs.StringVar(&cmd.token, "token", "", "Admin UI/API access token. Auto-generated and persisted at "+
		"~/.watchersid/token if left empty.")
	fs.BoolVar(&cmd.noAuth, "no-auth", false, "Disable the admin token check. Not recommended unless you're "+
		"putting your own auth in front of WatchersID.")
	fs.BoolVar(&cmd.chrome, "chrome", false, "Launch Chrome with proxy settings applied and certificate errors ignored.")
	fs.BoolVar(&cmd.version, "version", false, "Output version.")
	fs.BoolVar(&cmd.version, "v", false, "Output version.")

	cmd.config.RegisterFlags(fs)

	return &ffcli.Command{
		Name:    "watchersid",
		FlagSet: fs,
		Subcommands: []*ffcli.Command{
			NewCertCommand(cmd.config),
		},
		Exec: cmd.Exec,
		UsageFunc: func(*ffcli.Command) string {
			return watchersidUsage
		},
	}, cmd.config
}

func (cmd *WatchersIDCommand) Exec(ctx context.Context, _ []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	if cmd.version {
		fmt.Fprint(os.Stdout, version+"\n")
		return nil
	}

	mainLogger := cmd.config.logger.Named("main")

	listenHost, listenPort, err := net.SplitHostPort(cmd.addr)
	if err != nil {
		mainLogger.Fatal("Failed to parse listening address.", zap.Error(err))
	}

	url := fmt.Sprintf("http://%v:%v", listenHost, listenPort)
	if listenHost == "" || listenHost == "0.0.0.0" || listenHost == "127.0.0.1" || listenHost == "::1" {
		url = fmt.Sprintf("http://localhost:%v", listenPort)
	}

	// Expand `~` in filepaths.
	caCertFile, err := homedir.Expand(cmd.cert)
	if err != nil {
		cmd.config.logger.Fatal("Failed to parse CA certificate filepath.", zap.Error(err))
	}

	caKeyFile, err := homedir.Expand(cmd.key)
	if err != nil {
		cmd.config.logger.Fatal("Failed to parse CA private key filepath.", zap.Error(err))
	}

	dbPath, err := homedir.Expand(cmd.db)
	if err != nil {
		cmd.config.logger.Fatal("Failed to parse database path.", zap.Error(err))
	}

	// Load existing CA certificate and key from disk, or generate and write
	// to disk if no files exist yet.
	caCert, caKey, err := proxy.LoadOrCreateCA(caKeyFile, caCertFile)
	if err != nil {
		cmd.config.logger.Fatal("Failed to load or create CA key pair.", zap.Error(err))
	}

	dbLogger := cmd.config.logger.Named("boltdb").Sugar()
	boltOpts := *bbolt.DefaultOptions
	boltOpts.Logger = &bolt.Logger{SugaredLogger: dbLogger}

	boltDB, err := bolt.OpenDatabase(dbPath, &boltOpts)
	if err != nil {
		cmd.config.logger.Fatal("Failed to open database.", zap.Error(err))
	}
	defer boltDB.Close()

	scope := &scope.Scope{}

	reqLogService := reqlog.NewService(reqlog.Config{
		Scope:      scope,
		Repository: boltDB,
		Logger:     cmd.config.logger.Named("reqlog").Sugar(),
	})

	interceptService := intercept.NewService(intercept.Config{
		Logger: cmd.config.logger.Named("intercept").Sugar(),
	})

	senderService := sender.NewService(sender.Config{
		Repository:    boltDB,
		ReqLogService: reqLogService,
	})

	projService, err := proj.NewService(proj.Config{
		Repository:       boltDB,
		InterceptService: interceptService,
		ReqLogService:    reqLogService,
		SenderService:    senderService,
		Scope:            scope,
	})
	if err != nil {
		cmd.config.logger.Fatal("Failed to create new projects service.", zap.Error(err))
	}

	proxy, err := proxy.NewProxy(proxy.Config{
		CACert: caCert,
		CAKey:  caKey,
		Logger: cmd.config.logger.Named("proxy").Sugar(),
	})
	if err != nil {
		cmd.config.logger.Fatal("Failed to create new proxy.", zap.Error(err))
	}

	proxy.UseRequestModifier(reqLogService.RequestModifier)
	proxy.UseResponseModifier(reqLogService.ResponseModifier)
	proxy.UseRequestModifier(interceptService.RequestModifier)
	proxy.UseResponseModifier(interceptService.ResponseModifier)

	fsSub, err := fs.Sub(adminContent, "admin")
	if err != nil {
		cmd.config.logger.Fatal("Failed to construct file system subtree from admin dir.", zap.Error(err))
	}

	adminHandler := http.FileServer(http.FS(fsSub))
	router := mux.NewRouter().SkipClean(true)
	adminRouter := router.MatcherFunc(func(req *http.Request, match *mux.RouteMatch) bool {
		hostname, _ := os.Hostname()
		host, _, _ := net.SplitHostPort(req.Host)

		// Serve local admin routes when either:
		// - The `Host` is well-known, e.g. `watchersid.proxy`, `localhost:[port]`
		//   or the listen addr `[host]:[port]`.
		// - The request is not for TLS proxying (e.g. no `CONNECT`) and not
		//   for proxying an external URL. E.g. Request-Line (RFC 7230, Section 3.1.1)
		//   has no scheme.
		return strings.EqualFold(host, hostname) ||
			req.Host == "watchersid.proxy" ||
			req.Host == fmt.Sprintf("%v:%v", "localhost", listenPort) ||
			req.Host == fmt.Sprintf("%v:%v", listenHost, listenPort) ||
			req.Method != http.MethodConnect && !strings.HasPrefix(req.RequestURI, "http://")
	}).Subrouter().StrictSlash(true)

	// GraphQL server.
	gqlEndpoint := "/api/graphql/"
	var gqlHandler http.Handler = api.HTTPHandler(&api.Resolver{
		ProjectService:    projService,
		RequestLogService: reqLogService,
		InterceptService:  interceptService,
		SenderService:     senderService,
	}, gqlEndpoint)

	var adminUIHandler http.Handler = adminHandler

	// Protect the admin UI and GraphQL API with a token, so exposing
	// WatchersID beyond localhost doesn't hand out free access to
	// intercepted traffic and projects. Skippable via --no-auth.
	if !cmd.noAuth {
		token, err := loadOrCreateToken(cmd.token)
		if err != nil {
			mainLogger.Fatal("Failed to load or create admin access token.", zap.Error(err))
		}

		gqlHandler = requireToken(token, gqlHandler)
		adminUIHandler = requireToken(token, adminUIHandler)

		mainLogger.Info(fmt.Sprintf("Admin access token: %v", token))
		mainLogger.Info("Use this as the password (username can be blank) when your browser or client prompts for auth.")
	} else {
		mainLogger.Info("Admin authentication is DISABLED (--no-auth). The admin UI and API are unprotected.")
	}

	adminRouter.Path(gqlEndpoint).Handler(gqlHandler)

	// Admin interface.
	adminRouter.PathPrefix("").Handler(adminUIHandler)

	// Fallback (default) is the Proxy handler.
	router.PathPrefix("").Handler(proxy)

	httpServer := &http.Server{
		Addr:         cmd.addr,
		Handler:      router,
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}, // Disable HTTP/2
		ErrorLog:     zap.NewStdLog(cmd.config.logger.Named("http")),
	}

	go func() {
		mainLogger.Info(fmt.Sprintf("Watchers ID (v%v) is running on %v ...", version, cmd.addr))
		mainLogger.Info(fmt.Sprintf("\x1b[%dm%s\x1b[0m", uint8(32), "Get started at "+url))

		err := httpServer.ListenAndServe()
		if err != http.ErrServerClosed {
			mainLogger.Fatal("HTTP server closed unexpected.", zap.Error(err))
		}
	}()

	if cmd.chrome {
		ctx, cancel := chrome.NewExecAllocator(ctx, chrome.Config{
			ProxyServer:      url,
			ProxyBypassHosts: []string{url},
		})
		defer cancel()

		taskCtx, cancel := chromedp.NewContext(ctx)
		defer cancel()

		err = chromedp.Run(taskCtx, chromedp.Navigate(url))

		switch {
		case errors.Is(err, exec.ErrNotFound):
			mainLogger.Info("Chrome executable not found.")
		case err != nil:
			mainLogger.Error(fmt.Sprintf("Failed to navigate to %v.", url), zap.Error(err))
		default:
			mainLogger.Info("Launched Chrome.")
		}
	}

	// Wait for interrupt signal.
	<-ctx.Done()
	// Restore signal, allowing "force quit".
	stop()

	mainLogger.Info("Shutting down HTTP server. Press Ctrl+C to force quit.")

	// Note: We expect httpServer.Handler to handle timeouts, thus, we don't
	// need a context value with deadline here.
	//nolint:contextcheck
	err = httpServer.Shutdown(context.Background())
	if err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %w", err)
	}

	return nil
}
