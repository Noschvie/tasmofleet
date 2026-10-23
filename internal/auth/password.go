package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Password hashing parameters (spec §15.1).
const (
	PasswordIterations = 600000
	PasswordMinLength  = 12
	saltLen            = 16
	keyLen             = 32
	maxIterations      = 10_000_000 // bounds the work a corrupted hash can cause
)

// Password errors.
var (
	ErrWrongPassword = errors.New("auth: wrong password")
	ErrInvalidHash   = errors.New("auth: invalid password hash")
	ErrWeakPassword  = fmt.Errorf("auth: password shorter than %d characters", PasswordMinLength)
)

var b64 = base64.RawStdEncoding

// HashPassword returns a PHC-style PBKDF2 hash string:
// $pbkdf2-sha256$i=600000$<salt>$<hash> (unpadded standard base64).
func HashPassword(password string) (string, error) {
	if len([]rune(password)) < PasswordMinLength {
		return "", ErrWeakPassword
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, PasswordIterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$pbkdf2-sha256$i=%d$%s$%s", PasswordIterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks password against a HashPassword string in constant
// time. It returns nil, ErrWrongPassword or ErrInvalidHash.
func VerifyPassword(hash, password string) error {
	parts := strings.Split(hash, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "pbkdf2-sha256" || !strings.HasPrefix(parts[2], "i=") {
		return ErrInvalidHash
	}
	iter, err := strconv.Atoi(strings.TrimPrefix(parts[2], "i="))
	if err != nil || iter < 1 || iter > maxIterations {
		return ErrInvalidHash
	}
	salt, err1 := b64.DecodeString(parts[3])
	want, err2 := b64.DecodeString(parts[4])
	if err1 != nil || err2 != nil || len(salt) == 0 || len(want) == 0 {
		return ErrInvalidHash
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return ErrInvalidHash
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrWrongPassword
	}
	return nil
}

// dummyHash is verified against when a username does not exist, so the
// response time does not reveal which usernames exist.
var dummyHash = "$pbkdf2-sha256$i=600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
