package main

import (
	"anytls/user"
	"crypto/tls"
)

type myServer struct {
	tlsConfig   *tls.Config
	userManager *user.Manager
}

func NewMyServer(tlsConfig *tls.Config, userManager *user.Manager) *myServer {
	s := &myServer{
		tlsConfig:   tlsConfig,
		userManager: userManager,
	}
	return s
}
