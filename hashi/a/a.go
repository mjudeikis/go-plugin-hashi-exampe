package main

import (
	plugin "github.com/hashicorp/go-plugin"
	"github.com/mjudeikis/go-plugin-hashi-exampe/hashi"
)

type A struct{}

func (a A) SayHello(n string) (string, error) {
	return "hello, " + n, nil
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: hashi.Handshake,
		Plugins: plugin.PluginSet{
			"a": &hashi.P{&A{}},
		},
	})
}
