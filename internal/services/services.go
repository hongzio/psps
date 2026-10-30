// Package services lists the credential proxies gero runs, and what is
// turned on per isolation the way they are: shared folders, forwarded
// ports, and mail between agents.
package services

import (
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/awsproxy"
	"github.com/hongzio/gero/internal/proxy/chproxy"
	"github.com/hongzio/gero/internal/proxy/ddproxy"
	"github.com/hongzio/gero/internal/proxy/gitproxy"
	"github.com/hongzio/gero/internal/proxy/grafanaproxy"
	"github.com/hongzio/gero/internal/proxy/linearproxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/proxy/notionproxy"
	"github.com/hongzio/gero/internal/proxy/pgproxy"
	"github.com/hongzio/gero/internal/proxy/portproxy"
	"github.com/hongzio/gero/internal/proxy/shareproxy"
	"github.com/hongzio/gero/internal/proxy/slackproxy"
)

// Registry is every credential proxy, in the order they are listed.
func Registry() *proxy.Registry {
	return proxy.NewRegistry(
		gitproxy.Service{},
		awsproxy.Service{},
		pgproxy.Service{},
		chproxy.Service{},
		ddproxy.Service{},
		grafanaproxy.Service{},
		slackproxy.Service{},
		notionproxy.Service{},
		linearproxy.Service{},
		shareproxy.Service{},
		portproxy.Service{},
		mailproxy.Service{},
	)
}
