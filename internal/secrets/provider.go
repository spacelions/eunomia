package secrets

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrSecretNotFound is returned when a requested secret does not exist.
var ErrSecretNotFound = errors.New("secret not found")

// SecretMetadata describes a stored secret without exposing its plaintext value.
type SecretMetadata struct {
	TenantID  string
	Connector string
	Key       string
	KeyID     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SecretProvider stores and retrieves tenant-scoped connector credentials.
type SecretProvider interface {
	Get(ctx context.Context, tenantID, connector, key string) ([]byte, error)
	Set(ctx context.Context, tenantID, connector, key string, value []byte) error
	List(ctx context.Context, tenantID string) ([]SecretMetadata, error)
	Delete(ctx context.Context, tenantID, connector, key string) error
}

// Envelope holds the encrypted components of a single secret.
type Envelope struct {
	KeyID        string
	EncryptedDEK []byte
	Nonce        []byte
	Ciphertext   []byte
}

func secretAAD(tenantID, connector, key string) []byte {
	return []byte(tenantID + "\x00" + connector + "\x00" + key)
}

func validateCoordinates(tenantID, connector, key string) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("tenant_id is required")
	}
	if strings.TrimSpace(connector) == "" {
		return errors.New("connector is required")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("secret key is required")
	}
	return nil
}

// EncryptEnvelope generates a unique 32-byte DEK, encrypts plaintext using AES-256-GCM
// with AAD bound to (tenantID, connector, key), and wraps the DEK via wrapper.
func EncryptEnvelope(ctx context.Context, wrapper KeyWrapper, tenantID, connector, key string, plaintext []byte) (*Envelope, error) {
	if err := validateCoordinates(tenantID, connector, key); err != nil {
		return nil, err
	}
	if len(plaintext) == 0 {
		return nil, errors.New("secret value must not be empty")
	}

	dek := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("generating DEK: %w", err)
	}
	defer zeroBytes(dek)

	gcm := mustGCM(dek)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating secret nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, secretAAD(tenantID, connector, key))

	keyID, wrappedDEK, err := wrapper.Wrap(ctx, dek)
	if err != nil {
		return nil, fmt.Errorf("wrapping DEK: %w", err)
	}

	return &Envelope{
		KeyID:        keyID,
		EncryptedDEK: wrappedDEK,
		Nonce:        nonce,
		Ciphertext:   ciphertext,
	}, nil
}

// DecryptEnvelope unwraps the DEK and decrypts the secret ciphertext verifying AAD binding.
func DecryptEnvelope(ctx context.Context, wrapper KeyWrapper, tenantID, connector, key string, env Envelope) ([]byte, error) {
	if err := validateCoordinates(tenantID, connector, key); err != nil {
		return nil, err
	}
	dek, err := wrapper.Unwrap(ctx, env.KeyID, env.EncryptedDEK)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(dek)

	gcm := mustGCM(dek)
	plaintext, err := gcm.Open(nil, env.Nonce, env.Ciphertext, secretAAD(tenantID, connector, key))
	if err != nil {
		return nil, fmt.Errorf("decrypting secret %s/%s/%s: %w", tenantID, connector, key, err)
	}
	return plaintext, nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
