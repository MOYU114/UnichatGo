package assistant

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const apiTokenKeyEnv = "UNICHATGO_APIKEY_KEY"

var errInvalidCiphertext = errors.New("invalid token ciphertext")

type tokenCipher struct {
	aead cipher.AEAD
}

func newTokenCipherFromEnv() (*tokenCipher, error) {
	raw := strings.TrimSpace(os.Getenv(apiTokenKeyEnv))
	if raw == "" {
		return nil, fmt.Errorf("%s not set", apiTokenKeyEnv)
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", apiTokenKeyEnv, err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &tokenCipher{aead: aead}, nil
}

func decodeKey(raw string) ([]byte, error) {
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid key length %d, want 32", len(key))
	}
	return key, nil
}

func (c *tokenCipher) Encrypt(plain string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	cipherText := c.aead.Seal(nil, nonce, []byte(plain), nil)
	buf := append(nonce, cipherText...)
	return base64.StdEncoding.EncodeToString(buf), nil
}

func (c *tokenCipher) Decrypt(input string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(input)
	if err != nil {
		return "", errInvalidCiphertext
	}
	ns := c.aead.NonceSize()
	if len(data) < ns {
		return "", errInvalidCiphertext
	}
	nonce := data[:ns]
	cipherText := data[ns:]
	plain, err := c.aead.Open(nil, nonce, cipherText, nil)
	if err != nil {
		return "", errInvalidCiphertext
	}
	return string(plain), nil
}
