package hashi

import (
	"net/rpc"

	plugin "github.com/hashicorp/go-plugin"
)

// Handshake is for ensuring the plugin is known/supported
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "HASHI",
	MagicCookieValue: "hola",
}

// Plug is the interface that clients (main) will call and servers (plugins) will implement
type Plug interface {
	SayHello(string) (string, error)
}

// P satisfies the hashicorp/go-plugin.Plugin interface for instantiating clients / servers
type P struct {
	Plug
}

func (p *P) Server(*plugin.MuxBroker) (interface{}, error) {
	return &Server{p.Plug}, nil
}

func (p *P) Client(m *plugin.MuxBroker, c *rpc.Client) (interface{}, error) {
	return &Client{c}, nil
}

// Server is a shim for converting a net/rpc call to our interface
type Server struct {
	p Plug
}

func (s *Server) SayHello(n string, r *string) (err error) {
	*r, err = s.p.SayHello(n)
	return
}

// Client is a shim for converting our nice interface to a net/rpc call
type Client struct {
	c *rpc.Client
}

func (c *Client) SayHello(n string) (string, error) {
	var r string
	// Plugin is the object hashicorp/go-plugin registers
	err := c.c.Call("Plugin.SayHello", n, &r)
	return r, err
}
