package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UpstreamDialer connects to a target host:port via an upstream proxy.
type UpstreamDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// ProxyDialer manages dialing through SOCKS5, SOCKS5h, or HTTP proxies.
type ProxyDialer struct {
	ProxyURL *url.URL
	Timeout  time.Duration
}

// NewProxyDialer creates a dialer for the given proxy URL.
func NewProxyDialer(rawURL string, timeout time.Duration) (*ProxyDialer, error) {
	rawURL = strings.TrimSpace(rawURL)
	if strings.EqualFold(rawURL, "direct") || strings.EqualFold(rawURL, "direct://") || strings.HasPrefix(strings.ToLower(rawURL), "direct://") {
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		return &ProxyDialer{
			ProxyURL: &url.URL{Scheme: "direct", Host: "direct"},
			Timeout:  timeout,
		}, nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %w", rawURL, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "direct":
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		return &ProxyDialer{
			ProxyURL: parsed,
			Timeout:  timeout,
		}, nil
	case "socks5", "socks5h", "http", "https":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (supported: socks5, socks5h, http, direct)", scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("proxy URL %q is missing host:port", rawURL)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &ProxyDialer{
		ProxyURL: parsed,
		Timeout:  timeout,
	}, nil
}

// DialContext establishes a connection to targetAddr via the configured upstream proxy or directly.
func (d *ProxyDialer) DialContext(ctx context.Context, network, targetAddr string) (net.Conn, error) {
	scheme := strings.ToLower(d.ProxyURL.Scheme)
	switch scheme {
	case "direct":
		dialer := &net.Dialer{Timeout: d.Timeout}
		return dialer.DialContext(ctx, "tcp", targetAddr)
	case "socks5", "socks5h":
		return d.dialSOCKS5(ctx, targetAddr)
	case "http", "https":
		return d.dialHTTP(ctx, targetAddr)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", scheme)
	}
}

// dialSOCKS5 performs a pure Go RFC 1928 / RFC 1929 handshake over TCP.
func (d *ProxyDialer) dialSOCKS5(ctx context.Context, targetAddr string) (net.Conn, error) {
	proxyHost := d.ProxyURL.Host
	if !strings.Contains(proxyHost, ":") {
		proxyHost += ":1080"
	}

	dialer := &net.Dialer{Timeout: d.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", proxyHost)
	if err != nil {
		return nil, fmt.Errorf("connecting to SOCKS5 proxy %s: %w", proxyHost, err)
	}

	// Apply deadline for the handshake
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(d.Timeout))
	}
	defer func() {
		_ = conn.SetDeadline(time.Time{}) // clear handshake deadline on return
	}()

	var user, pass string
	if d.ProxyURL.User != nil {
		user = d.ProxyURL.User.Username()
		pass, _ = d.ProxyURL.User.Password()
	}

	// 1. Send Version & Authentication Methods
	var methods []byte
	if user != "" || pass != "" {
		methods = []byte{0x05, 0x02, 0x00, 0x02} // Version 5, 2 methods: No Auth (0x00) & User/Pass (0x02)
	} else {
		methods = []byte{0x05, 0x01, 0x00} // Version 5, 1 method: No Auth (0x00)
	}

	if _, err := conn.Write(methods); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing SOCKS5 greeting: %w", err)
	}

	// 2. Read Server Method Selection
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading SOCKS5 greeting reply: %w", err)
	}
	if reply[0] != 0x05 {
		conn.Close()
		return nil, fmt.Errorf("invalid SOCKS version %d (expected 5)", reply[0])
	}

	switch reply[1] {
	case 0x00:
		// No authentication required
	case 0x02:
		// Username/Password authentication (RFC 1929)
		if user == "" && pass == "" {
			conn.Close()
			return nil, errors.New("SOCKS5 server requested auth, but no credentials provided")
		}
		if len(user) > 255 || len(pass) > 255 {
			conn.Close()
			return nil, errors.New("SOCKS5 username or password exceeds 255 bytes")
		}
		authBuf := make([]byte, 0, 3+len(user)+len(pass))
		authBuf = append(authBuf, 0x01) // Auth subnegotiation version 1
		authBuf = append(authBuf, byte(len(user)))
		authBuf = append(authBuf, user...)
		authBuf = append(authBuf, byte(len(pass)))
		authBuf = append(authBuf, pass...)

		if _, err := conn.Write(authBuf); err != nil {
			conn.Close()
			return nil, fmt.Errorf("sending SOCKS5 auth: %w", err)
		}

		authReply := make([]byte, 2)
		if _, err := io.ReadFull(conn, authReply); err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading SOCKS5 auth reply: %w", err)
		}
		if authReply[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("SOCKS5 authentication failed with code 0x%02x", authReply[1])
		}
	case 0xFF:
		conn.Close()
		return nil, errors.New("SOCKS5 server: no acceptable authentication methods (0xFF)")
	default:
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 server chose unsupported auth method 0x%02x", reply[1])
	}

	// 3. Send CONNECT Request
	targetHost, targetPortStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("invalid target address %q: %w", targetAddr, err)
	}
	targetPort, err := strconv.Atoi(targetPortStr)
	if err != nil || targetPort < 1 || targetPort > 65535 {
		conn.Close()
		return nil, fmt.Errorf("invalid target port %q: %w", targetPortStr, err)
	}

	reqBuf := make([]byte, 0, 300)
	reqBuf = append(reqBuf, 0x05, 0x01, 0x00) // VER=5, CMD=1 (CONNECT), RSV=0

	// Determine address type
	isSocks5h := strings.ToLower(d.ProxyURL.Scheme) == "socks5h"
	ip := net.ParseIP(targetHost)

	if ip != nil && !isSocks5h {
		if ipv4 := ip.To4(); ipv4 != nil {
			reqBuf = append(reqBuf, 0x01) // ATYP = IPv4
			reqBuf = append(reqBuf, ipv4...)
		} else {
			reqBuf = append(reqBuf, 0x04) // ATYP = IPv6
			reqBuf = append(reqBuf, ip.To16()...)
		}
	} else {
		// Domain name (remote resolution)
		if len(targetHost) > 255 {
			conn.Close()
			return nil, fmt.Errorf("domain name %q exceeds 255 bytes", targetHost)
		}
		reqBuf = append(reqBuf, 0x03) // ATYP = Domain
		reqBuf = append(reqBuf, byte(len(targetHost)))
		reqBuf = append(reqBuf, targetHost...)
	}

	// Port (2 bytes big endian)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(targetPort))
	reqBuf = append(reqBuf, portBuf...)

	if _, err := conn.Write(reqBuf); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending SOCKS5 CONNECT: %w", err)
	}

	// 4. Read Response Header
	respHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, respHeader); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading SOCKS5 CONNECT reply header: %w", err)
	}
	if respHeader[0] != 0x05 {
		conn.Close()
		return nil, fmt.Errorf("invalid SOCKS reply version %d", respHeader[0])
	}
	if respHeader[1] != 0x00 {
		conn.Close()
		return nil, socks5Error(respHeader[1])
	}

	// Read and discard bound address
	switch respHeader[3] {
	case 0x01: // IPv4 (4 bytes) + port (2 bytes)
		discard := make([]byte, 6)
		if _, err := io.ReadFull(conn, discard); err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading SOCKS5 IPv4 bound address: %w", err)
		}
	case 0x03: // Domain: 1 byte len + domain + port (2 bytes)
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading SOCKS5 domain bound address length: %w", err)
		}
		discard := make([]byte, int(lenBuf[0])+2)
		if _, err := io.ReadFull(conn, discard); err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading SOCKS5 domain bound address: %w", err)
		}
	case 0x04: // IPv6 (16 bytes) + port (2 bytes)
		discard := make([]byte, 18)
		if _, err := io.ReadFull(conn, discard); err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading SOCKS5 IPv6 bound address: %w", err)
		}
	default:
		conn.Close()
		return nil, fmt.Errorf("unsupported SOCKS5 bound address type 0x%02x", respHeader[3])
	}

	return conn, nil
}

// dialHTTP performs an HTTP CONNECT tunnel to targetAddr via an upstream HTTP proxy.
func (d *ProxyDialer) dialHTTP(ctx context.Context, targetAddr string) (net.Conn, error) {
	proxyHost := d.ProxyURL.Host
	if !strings.Contains(proxyHost, ":") {
		if strings.ToLower(d.ProxyURL.Scheme) == "https" {
			proxyHost += ":443"
		} else {
			proxyHost += ":80"
		}
	}

	dialer := &net.Dialer{Timeout: d.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", proxyHost)
	if err != nil {
		return nil, fmt.Errorf("connecting to HTTP proxy %s: %w", proxyHost, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(d.Timeout))
	}
	defer func() {
		_ = conn.SetDeadline(time.Time{})
	}()

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetAddr, targetAddr)
	if d.ProxyURL.User != nil {
		auth := d.ProxyURL.User.String()
		basicAuth := base64.StdEncoding.EncodeToString([]byte(auth))
		connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", basicAuth)
	}
	connectReq += "Proxy-Connection: Keep-Alive\r\n\r\n"

	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing HTTP CONNECT request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading HTTP CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("HTTP proxy returned non-200 status: %s", resp.Status)
	}

	return conn, nil
}

func socks5Error(code byte) error {
	switch code {
	case 0x01:
		return errors.New("SOCKS5 error: general SOCKS server failure (0x01)")
	case 0x02:
		return errors.New("SOCKS5 error: connection not allowed by ruleset (0x02)")
	case 0x03:
		return errors.New("SOCKS5 error: network unreachable (0x03)")
	case 0x04:
		return errors.New("SOCKS5 error: host unreachable (0x04)")
	case 0x05:
		return errors.New("SOCKS5 error: connection refused by destination (0x05)")
	case 0x06:
		return errors.New("SOCKS5 error: TTL expired (0x06)")
	case 0x07:
		return errors.New("SOCKS5 error: command not supported (0x07)")
	case 0x08:
		return errors.New("SOCKS5 error: address type not supported (0x08)")
	default:
		return fmt.Errorf("SOCKS5 error: unknown failure code 0x%02x", code)
	}
}
