package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"time"
)

// Signer makes and checks expiring stream links.
//
// Media players (mpv, AVFoundation) can't easily send an Authorization
// header, so stream URLs carry their own proof instead: an expiry time and an
// HMAC-SHA256 over the file ID and that expiry. A link that leaks into a
// player's history or a log stops working when it expires, and can't be
// altered to point at another file or last longer.
type Signer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

func NewSigner(key []byte, ttl time.Duration) *Signer {
	return &Signer{key: key, ttl: ttl, now: time.Now}
}

func (s *Signer) mac(id string, exp int64) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id))
	m.Write([]byte{0})
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Sign returns the expiry and signature for a file ID.
func (s *Signer) Sign(id string) (exp int64, sig string) {
	exp = s.now().Add(s.ttl).Unix()
	return exp, s.mac(id, exp)
}

// Verify checks a signature in constant time and that it hasn't expired.
func (s *Signer) Verify(id, expStr, sig string) bool {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || s.now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.mac(id, exp)))
}
