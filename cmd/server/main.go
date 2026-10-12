package main

import (
	"anytls/api"
	"anytls/proxy"
	"anytls/proxy/padding"
	"anytls/user"
	"anytls/util"
	"context"
	"crypto/tls"
	"flag"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

func main() {
	listen := flag.String("l", "0.0.0.0:8443", "server listen port")
	password := flag.String("p", "", "password (bootstraps a single default user if the user database is empty)")
	paddingScheme := flag.String("padding-scheme", "", "padding-scheme")
	dbPath := flag.String("db", "anytls.db", "path to the user database (sqlite)")
	apiListen := flag.String("api-listen", "", "admin API listen address, e.g. 127.0.0.1:8080 (disabled if empty)")
	apiKey := flag.String("api-key", "", "static API key required by the admin API (required if -api-listen is set)")
	certFile := flag.String("cert", "", "TLS certificate file (PEM); requires -key. Falls back to a generated self-signed cert if unset")
	keyFile := flag.String("key", "", "TLS private key file (PEM); requires -cert")
	fallbackAddr := flag.String("fallback", "", "local address to forward connections to when the anytls auth handshake fails, e.g. 127.0.0.1:80 (connection is just closed if unset)")
	flag.Parse()

	if *apiListen != "" && *apiKey == "" {
		logrus.Fatalln("please set -api-key when -api-listen is enabled")
	}
	if (*certFile == "") != (*keyFile == "") {
		logrus.Fatalln("-cert and -key must be set together")
	}

	store, err := user.OpenStore(*dbPath)
	if err != nil {
		logrus.Fatalln("open user database:", err)
	}

	userManager, err := user.NewManager(store)
	if err != nil {
		logrus.Fatalln("init user manager:", err)
	}

	// Outbound groups (direct dialing plus any configured SOCKS5 chain
	// proxies, managed via the admin API/web panel) are persisted in the
	// user database; register each one's live dialer before accepting any
	// connections. A group's URL was already validated when it was
	// created/updated, so a failure here means the DB row is corrupt -
	// log and skip it (falls back to direct for that group) rather than
	// refusing to start the whole server over one bad group.
	groups, err := userManager.ListOutboundGroups()
	if err != nil {
		logrus.Fatalln("load outbound groups:", err)
	}
	for _, g := range groups {
		if err := proxy.SetGroupSOCKS5(g.ID, g.SOCKS5URL); err != nil {
			logrus.Errorln("[Server] outbound group", g.Name, "has an invalid socks5_url, falling back to direct for it:", err)
			continue
		}
		logTarget := g.SOCKS5URL
		if i := strings.LastIndex(logTarget, "@"); i != -1 {
			logTarget = "***@" + logTarget[i+1:] // don't put the socks5 credentials in the log
		}
		logrus.Infoln("[Server] outbound group", g.Name, "chains through SOCKS5 server:", logTarget)
	}

	if *password != "" && userManager.UserCount() == 0 {
		if _, err := userManager.CreateUser(&user.User{
			Username:          "default",
			Password:          *password,
			Enabled:           true,
			TrafficResetCycle: user.ResetCycleNone,
		}); err != nil {
			logrus.Fatalln("bootstrap default user:", err)
		}
		logrus.Infoln("[Server] bootstrapped default user from -p (unlimited quota)")
	}
	if userManager.UserCount() == 0 {
		logrus.Fatalln("no users configured: set -p to bootstrap a default user, or create users via the admin API")
	}

	if *apiListen != "" {
		apiServer := api.NewServer(userManager, *apiKey, *listen)
		go func() {
			var err error
			if *certFile != "" {
				// Reuse the same real certificate as the anytls protocol
				// listener, so the admin page isn't stuck on plain HTTP
				// once a real -cert/-key is configured.
				err = apiServer.ListenAndServeTLS(*apiListen, *certFile, *keyFile)
			} else {
				err = apiServer.ListenAndServe(*apiListen)
			}
			if err != nil {
				logrus.Fatalln("admin API:", err)
			}
		}()
	}

	if *paddingScheme != "" {
		if f, err := os.Open(*paddingScheme); err == nil {
			b, err := io.ReadAll(f)
			if err != nil {
				logrus.Fatalln(err)
			}
			if padding.UpdatePaddingScheme(b) {
				logrus.Infoln("loaded padding scheme file:", *paddingScheme)
			} else {
				logrus.Errorln("wrong format padding scheme file:", *paddingScheme)
			}
			f.Close()
		} else {
			logrus.Fatalln(err)
		}
	}

	logLevel, err := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		logLevel = logrus.InfoLevel
	}
	logrus.SetLevel(logLevel)

	logrus.Infoln("[Server]", util.ProgramVersionName)
	logrus.Infoln("[Server] Listening TCP", *listen)

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		logrus.Fatalln("listen server tcp:", err)
	}

	var tlsCert *tls.Certificate
	if *certFile != "" {
		loaded, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			logrus.Fatalln("load TLS certificate:", err)
		}
		tlsCert = &loaded
		logrus.Infoln("[Server] using TLS certificate from", *certFile)
	} else {
		tlsCert, err = util.GenerateKeyPair(time.Now, "")
		if err != nil {
			logrus.Fatalln("generate self-signed certificate:", err)
		}
		logrus.Warnln("[Server] no -cert/-key given, using a generated self-signed certificate (clients must skip certificate verification)")
	}
	tlsConfig := &tls.Config{
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return tlsCert, nil
		},
	}

	ctx := context.Background()
	server := NewMyServer(tlsConfig, userManager, *fallbackAddr)

	for {
		c, err := listener.Accept()
		if err != nil {
			logrus.Fatalln("accept:", err)
		}
		go handleTcpConnection(ctx, c, server)
	}
}
