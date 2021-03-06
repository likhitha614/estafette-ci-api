package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// SecretHelper is the interface for encrypting and decrypting secrets
type SecretHelper interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
	EncryptEnvelope(string) (string, error)
	DecryptEnvelope(string) (string, error)
	DecryptAllEnvelopes(string) (string, error)
}

type secretHelperImpl struct {
	key string
}

// NewSecretHelper returns a new SecretHelper
func NewSecretHelper(key string) SecretHelper {
	return &secretHelperImpl{
		key: key,
	}
}

func (sh *secretHelperImpl) Encrypt(unencryptedText string) (encryptedTextPlusNonce string, err error) {
	// The key argument should be the AES key, either 16 or 32 bytes to select AES-128 or AES-256.
	key := []byte(sh.key)
	plaintext := []byte(unencryptedText)

	block, err := aes.NewCipher(key)
	if err != nil {
		return
	}

	// Never use more than 2^32 random nonces with a given key because of the risk of a repeat.
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return encryptedTextPlusNonce, err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return
	}

	ciphertext := aesgcm.Seal(nil, nonce, plaintext, nil)

	encryptedTextPlusNonce = fmt.Sprintf("%v.%v", base64.URLEncoding.EncodeToString(nonce), base64.URLEncoding.EncodeToString(ciphertext))

	return
}

func (sh *secretHelperImpl) Decrypt(encryptedTextPlusNonce string) (decryptedText string, err error) {

	splittedStrings := strings.Split(encryptedTextPlusNonce, ".")
	if splittedStrings == nil || len(splittedStrings) != 2 {
		err = errors.New("The encrypted text plus nonce doesn't split correctly")
		return
	}

	usedNonce := splittedStrings[0]
	encryptedText := splittedStrings[1]

	// The key argument should be the AES key, either 16 or 32 bytes to select AES-128 or AES-256.
	key := []byte(sh.key)
	ciphertext, _ := base64.URLEncoding.DecodeString(encryptedText)

	nonce, _ := base64.URLEncoding.DecodeString(usedNonce)

	block, err := aes.NewCipher(key)
	if err != nil {
		return
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return
	}

	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return
	}

	decryptedText = string(plaintext)

	return
}

func (sh *secretHelperImpl) EncryptEnvelope(unencryptedText string) (encryptedTextInEnvelope string, err error) {

	encryptedText, err := sh.Encrypt(unencryptedText)
	if err != nil {
		return
	}
	encryptedTextInEnvelope = fmt.Sprintf("estafette.secret(%v)", encryptedText)

	return
}

func (sh *secretHelperImpl) DecryptEnvelope(encryptedTextInEnvelope string) (decryptedText string, err error) {

	r, err := regexp.Compile(`^estafette\.secret\(([a-zA-Z0-9.=_-]+)\)$`)
	if err != nil {
		return
	}

	matches := r.FindStringSubmatch(encryptedTextInEnvelope)
	if matches == nil {
		return encryptedTextInEnvelope, nil
	}

	decryptedText, err = sh.Decrypt(matches[1])
	if err != nil {
		return
	}

	return
}

func (sh *secretHelperImpl) decryptEnvelopeInBytes(encryptedTextInEnvelope []byte) []byte {

	decryptedText, err := sh.DecryptEnvelope(string(encryptedTextInEnvelope))
	if err != nil {
		return nil
	}

	return []byte(decryptedText)
}

func (sh *secretHelperImpl) DecryptAllEnvelopes(encryptedTextWithEnvelopes string) (decryptedText string, err error) {

	r, err := regexp.Compile(`estafette\.secret\([a-zA-Z0-9.=_-]+\)`)
	if err != nil {
		return
	}

	decryptedText = string(r.ReplaceAllFunc([]byte(encryptedTextWithEnvelopes), sh.decryptEnvelopeInBytes))

	return
}
