package plugin

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Upstream HTTP client.
//
// The previous code used http.DefaultClient, which happily reuses idle
// keep-alive connections. A connection that an intermediate hop (NAT, VPN
// tunnel, HTTP proxy) has silently dropped is still considered "open" by the
// pool: the request is written, no response ever arrives, and the caller sees a
// 60s silent stall. Every observed first_frame_timeout on 2026-09-23 had that
// shape — an immediate retry on a fresh connection succeeded, and a direct curl
// (new connection each time) never reproduced it.
//
// Phase deadlines make a dead socket fail fast so the executor can retry on a
// fresh connection instead of waiting out the Stream Guard budget.
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
	idleConnTimeout       = 20 * time.Second
	maxIdleConnsPerHost   = 8
)

// upstreamHTTPClientOverride lets tests swap the client (httptest) without
// racing the sync.Once-built production transport. Nil in production.
var upstreamHTTPClientOverride *http.Client

var (
	upstreamClientOnce sync.Once
	upstreamHTTPClient *http.Client
)

// upstreamClient returns the shared client. Streamed responses legitimately last
// minutes, so there is no client-wide timeout; phases are bounded instead.
func upstreamClient() *http.Client {
	if c := upstreamHTTPClientOverride; c != nil {
		return c
	}
	upstreamClientOnce.Do(func() {
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       idleConnTimeout,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
		}
		upstreamHTTPClient = &http.Client{Transport: transport}
	})
	return upstreamHTTPClient
}

// dropIdleUpstreamConnections clears the pool so the next attempt cannot reuse a
// connection that an intermediate hop has already discarded.
func dropIdleUpstreamConnections() {
	upstreamClient().CloseIdleConnections()
}
