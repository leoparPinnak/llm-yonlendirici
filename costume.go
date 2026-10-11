package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// hopHeaders are connection-scoped and never cross either direction.
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// identityHeaders carry credentials or client identity the gateway replaces
// with its own costume.
var identityHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"Api-Key",
	"X-Goog-Api-Key",
	"Cookie",
	"B3",
	"Traceparent",
	"Tracestate",
}

// removeHopByHop deletes connection-scoped headers, including any the
// Connection header names.
func removeHopByHop(head http.Header) {
	for _, value := range head.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				head.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		head.Del(name)
	}
}

// costume is the OpenCode client identity the gateway wears upstream. The
// project hash is minted once per run; the session id per request.
type costume struct {
	userAgent string
	project   string
}

// newCostume mints the per-run identity: a SHA-1-shaped project hash, the
// shape the live gate accepts.
func newCostume(userAgent string) *costume {
	seed := make([]byte, 16)
	if _, err := rand.Read(seed); err != nil {
		seed = time.Now().AppendFormat(nil, "20060102150405.000000000")
	}
	sum := sha1.Sum(seed)
	return &costume{userAgent: userAgent, project: hex.EncodeToString(sum[:])}
}

// sessionID mints a fresh session identifier: "ses_" followed by 26
// characters — twelve hex, then fourteen mixed-case alphanumeric — the format
// the live gate accepts for an unregistered session.
func sessionID() string {
	hexSeed := make([]byte, 6)
	if _, err := rand.Read(hexSeed); err != nil {
		hexSeed = []byte("zenkos")
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	tail := make([]byte, 14)
	draw := make([]byte, 14)
	if _, err := rand.Read(draw); err != nil {
		for i := range tail {
			tail[i] = alphabet[i%len(alphabet)]
		}
	} else {
		for i, b := range draw {
			tail[i] = alphabet[int(b)%len(alphabet)]
		}
	}
	return "ses_" + hex.EncodeToString(hexSeed) + string(tail)
}

// dress turns head into the upstream request headers for one exchange. The
// client's own credentials never cross: the gateway owns the identity.
func (c *costume) dress(head http.Header) {
	removeHopByHop(head)
	for _, name := range identityHeaders {
		head.Del(name)
	}
	head.Set("Authorization", "Bearer public")
	head.Set("User-Agent", c.userAgent)
	head.Set("Accept", "*/*")
	head.Set("Content-Type", "application/json")
	head.Set("X-Opencode-Client", "cli")
	head.Set("X-Opencode-Project", c.project)
	session := sessionID()
	head.Set("X-Opencode-Session", session)
	head.Set("X-Session-Id", session)
	head.Set("X-Session-Affinity", session)
}
