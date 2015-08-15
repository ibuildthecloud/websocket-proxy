// This code is adapted from:
// https://github.com/armon/go-proxyproto
//
// The MIT License (MIT)
//
// Copyright (c) 2014 Armon Dadgar
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
package proxyprotocol

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
)

var (
	// prefix is the string we look for at the start of a connection
	// to check if this connection is using the proxy protocol
	prefix    = []byte("PROXY ")
	prefixLen = len(prefix)
)

// Listener is used to wrap an underlying listener,
// whose connections may be using the HAProxy Proxy Protocol (version 1).
// If the connection is using the protocol, the RemoteAddr() will return
// the correct client address.
type Listener struct {
	Listener net.Listener
}

// Conn is used to wrap and underlying connection which
// may be speaking the Proxy Protocol. If it is, the RemoteAddr() will
// return the address of the client instead of the proxy address.
type Conn struct {
	bufReader *bufio.Reader
	conn      net.Conn
	srcAddr   *net.TCPAddr
	once      sync.Once
}

type ProxyProtoInfo struct {
	Protocol   string
	ClientAddr *net.TCPAddr
	ProxyAddr  *net.TCPAddr
}

// Accept waits for and returns the next connection to the listener.
func (p *Listener) Accept() (net.Conn, error) {
	// Get the underlying connection
	conn, err := p.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return NewConn(conn), nil
}

// Close closes the underlying listener.
func (p *Listener) Close() error {
	return p.Listener.Close()
}

// Addr returns the underlying listener's network address.
func (p *Listener) Addr() net.Addr {
	return p.Listener.Addr()
}

// NewConn is used to wrap a net.Conn that may be speaking
// the proxy protocol into a proxyproto.Conn
func NewConn(conn net.Conn) *Conn {
	pConn := &Conn{
		bufReader: bufio.NewReader(conn),
		conn:      conn,
	}
	return pConn
}

// Read is check for the proxy protocol header when doing
// the initial scan. If there is an error parsing the header,
// it is returned and the socket is closed.
func (p *Conn) Read(b []byte) (int, error) {
	var err error
	p.once.Do(func() { err = p.checkPrefix() })
	if err != nil {
		return 0, err
	}
	return p.bufReader.Read(b)
}

func (p *Conn) Write(b []byte) (int, error) {
	return p.conn.Write(b)
}

func (p *Conn) Close() error {
	return p.conn.Close()
}

func (p *Conn) LocalAddr() net.Addr {
	return p.conn.LocalAddr()
}

// RemoteAddr returns the address of the client if the proxy
// protocol is being used, otherwise just returns the address of
// the socket peer. If there is an error parsing the header, the
// address of the client is not returned, and the socket is closed.
// Once implication of this is that the call could block if the
// client is slow. Using a Deadline is recommended if this is called
// before Read()
func (p *Conn) RemoteAddr() net.Addr {
	p.once.Do(func() {
		if err := p.checkPrefix(); err != nil && err != io.EOF {
			log.Warnf("[ERR] Failed to read proxy prefix: %v", err)
		}
	})
	if p.srcAddr != nil {
		return p.srcAddr
	}
	return p.conn.RemoteAddr()
}

func (p *Conn) SetDeadline(t time.Time) error {
	return p.conn.SetDeadline(t)
}

func (p *Conn) SetReadDeadline(t time.Time) error {
	return p.conn.SetReadDeadline(t)
}

func (p *Conn) SetWriteDeadline(t time.Time) error {
	return p.conn.SetWriteDeadline(t)
}

func (p *Conn) checkPrefix() error {
	// Incrementally check each byte of the prefix
	for i := 1; i <= prefixLen; i++ {
		inp, err := p.bufReader.Peek(i)
		if err != nil {
			return err
		}

		// Check for a prefix mis-match, quit early
		if !bytes.Equal(inp, prefix[:i]) {
			return nil
		}
	}

	// Read the header line
	header, err := p.bufReader.ReadString('\n')
	if err != nil {
		p.conn.Close()
		return err
	}

	// Strip the carriage return and new line
	header = header[:len(header)-2]

	// Split on spaces, should be (PROXY <type> <src addr> <dst addr> <src port> <dst port>)
	parts := strings.Split(header, " ")
	if len(parts) != 6 {
		p.conn.Close()
		return fmt.Errorf("Invalid header line: %s", header)
	}

	// Verify the type is known
	switch parts[1] {
	case "TCP4":
	case "TCP6":
	default:
		p.conn.Close()
		return fmt.Errorf("Unhandled address type: %s", parts[1])
	}

	// Parse out the source address
	ip := net.ParseIP(parts[2])
	if ip == nil {
		p.conn.Close()
		return fmt.Errorf("Invalid source ip: %s", parts[2])
	}
	port, err := strconv.Atoi(parts[4])
	if err != nil {
		p.conn.Close()
		return fmt.Errorf("Invalid source port: %s", parts[4])
	}
	p.srcAddr = &net.TCPAddr{IP: ip, Port: port}

	// Parse out the destination address
	ip = net.ParseIP(parts[3])
	if ip == nil {
		p.conn.Close()
		return fmt.Errorf("Invalid destination ip: %s", parts[3])
	}
	port, err = strconv.Atoi(parts[5])
	if err != nil {
		p.conn.Close()
		return fmt.Errorf("Invalid destination port: %s", parts[5])
	}

	destAddr := &net.TCPAddr{IP: ip, Port: port}
	proxyInfo := &ProxyProtoInfo{
		Protocol:   parts[1],
		ClientAddr: p.srcAddr,
		ProxyAddr:  destAddr,
	}
	putInfo(proxyInfo.ClientAddr.String(), proxyInfo)

	return nil
}
