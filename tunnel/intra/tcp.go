// Copyright 2019 The Outline Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Derived from go-tun2socks's "direct" handler under the Apache 2.0 license.

package intra

import (
	"io"
	"net"
	"time"

	"github.com/eycorsican/go-tun2socks/common/log"
	"github.com/eycorsican/go-tun2socks/core"

	"github.com/Jigsaw-Code/outline-go-tun2socks/tunnel/intra/doh"
	"github.com/Jigsaw-Code/outline-go-tun2socks/tunnel/intra/split"
)

// TCPHandler is a core TCP handler that also supports DOH and splitting control.
type TCPHandler interface {
	core.TCPConnHandler
	SetDNS(doh.Transport)
	SetAlwaysSplitHTTPS(bool)
}

type tcpHandler struct {
	TCPHandler
	fakedns          net.TCPAddr
	truedns          net.TCPAddr
	dns              doh.Atomic
	alwaysSplitHTTPS bool
	listener         TCPListener
}

// TCPSocketSummary provides information about each TCP socket, reported when it is closed.
type TCPSocketSummary struct {
	DownloadBytes int64 // Total bytes downloaded.
	UploadBytes   int64 // Total bytes uploaded.
	Duration      int32 // Duration in seconds.
	ServerPort    int16 // The server port.  All values except 80, 443, and 0 are set to -1.
	Synack        int32 // TCP handshake latency (ms)
	// Retry is non-nil if retry was possible.  Retry.Split is non-zero if a retry occurred.
	Retry *split.RetryStats
}

// TCPListener is notified when a socket closes.
type TCPListener interface {
	OnTCPSocketClosed(*TCPSocketSummary)
}

// NewTCPHandler returns a TCP forwarder with Intra-style behavior.
// Currently this class only redirects DNS traffic to a
// specified server.  (This should be rare for TCP.)
// All other traffic is forwarded unmodified.
func NewTCPHandler(fakedns, truedns net.TCPAddr, listener TCPListener) TCPHandler {
	return &tcpHandler{
		fakedns:  fakedns,
		truedns:  truedns,
		listener: listener,
	}
}

// TODO: Propagate TCP RST using local.Abort(), on appropriate errors.
func (h *tcpHandler) handleUpload(local core.TCPConn, remote split.DuplexConn, upload chan int64) {
	bytes, _ := remote.ReadFrom(local)
	local.CloseRead()
	remote.CloseWrite()
	upload <- bytes
}

func (h *tcpHandler) handleDownload(local core.TCPConn, remote split.DuplexConn) (bytes int64, err error) {
	bytes, err = io.Copy(local, remote)
	local.CloseWrite()
	remote.CloseRead()
	return
}

func (h *tcpHandler) forward(local net.Conn, remote split.DuplexConn, summary *TCPSocketSummary) {
	localtcp := local.(core.TCPConn)
	upload := make(chan int64)
	start := time.Now()
	go h.handleUpload(localtcp, remote, upload)
	download, _ := h.handleDownload(localtcp, remote)
	summary.DownloadBytes = download
	summary.UploadBytes = <-upload
	summary.Duration = int32(time.Since(start).Seconds())
	h.listener.OnTCPSocketClosed(summary)
}

func filteredPort(addr net.Addr) int16 {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return -1
	}
	if port == "80" {
		return 80
	}
	if port == "443" {
		return 443
	}
	if port == "0" {
		return 0
	}
	return -1
}

// TODO: Request upstream to make `conn` a `core.TCPConn` so we can avoid a type assertion.
func (h *tcpHandler) Handle(conn net.Conn, target *net.TCPAddr) error {
	// DNS override
	if target.IP.Equal(h.fakedns.IP) && target.Port == h.fakedns.Port {
		dns := h.dns.Load()
		if dns != nil {
			go doh.Accept(dns, conn)
			return nil
		}
		target = &h.truedns
	}
	var summary TCPSocketSummary
	summary.ServerPort = filteredPort(target)
	start := time.Now()
	var c split.DuplexConn
	var err error
	// TODO: Cancel dialing if c is closed.
	if summary.ServerPort == 443 {
		if h.alwaysSplitHTTPS {
			c, err = split.DialWithSplit(target)
		} else {
			summary.Retry = &split.RetryStats{}
			c, err = split.DialWithSplitRetry(target, split.DefaultTimeout, summary.Retry)
		}
	} else {
		c, err = net.DialTCP(target.Network(), nil, target)
	}
	if err != nil {
		return err
	}
	summary.Synack = int32(time.Since(start).Seconds() * 1000)
	go h.forward(conn, c, &summary)
	log.Infof("new proxy connection for target: %s:%s", target.Network(), target.String())
	return nil
}

func (h *tcpHandler) SetDNS(dns doh.Transport) {
	h.dns.Store(dns)
}

func (h *tcpHandler) SetAlwaysSplitHTTPS(s bool) {
	h.alwaysSplitHTTPS = s
}
