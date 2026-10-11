package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

type DualListener struct {
	net.Listener
	tlsConfig *tls.Config
}

type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

func NewDualListener(addr string) (*DualListener, error) {
	rawLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	cert, err := generateSelfSignedCert()
	if err != nil {
		rawLn.Close()
		return nil, fmt.Errorf("self-signed cert olusturulamadi: %w", err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	return &DualListener{
		Listener:  rawLn,
		tlsConfig: tlsConf,
	}, nil
}

func (l *DualListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	// İlk byte'ı oku: 0x16 ise TLS ClientHello (HTTPS), değilse düz HTTP
	buf := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c.Read(buf)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil || n == 0 {
		c.Close()
		return nil, fmt.Errorf("ilk paket okuma hatasi: %w", err)
	}

	mr := io.MultiReader(bytes.NewReader(buf[:n]), c)
	bConn := &bufferedConn{Conn: c, r: mr}

	if buf[0] == 0x16 {
		// HTTPS / TLS tespit edildi
		tlsConn := tls.Server(bConn, l.tlsConfig)
		return tlsConn, nil
	}

	// Düz HTTP bağlantısı
	return bConn, nil
}

func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"EVREN Local Proxy Router"},
			CommonName:   "127.0.0.1",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost", "127.0.0.1"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}, nil
}
