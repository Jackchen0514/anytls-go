package main

import (
	"anytls/user"
	"crypto/tls"
)

type myServer struct {
	tlsConfig    *tls.Config
	userManager  *user.Manager
	fallbackAddr string
}

func NewMyServer(tlsConfig *tls.Config, userManager *user.Manager, fallbackAddr string) *myServer {
	s := &myServer{
		tlsConfig:    tlsConfig,
		userManager:  userManager,
		fallbackAddr: fallbackAddr,
	}
	return s
}
