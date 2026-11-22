package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const keyBytes = 32 // AES-256

// KeyWrapper wraps and unwraps per-secret 32-byte Data Encryption Keys (DEKs).
// In v0.1, LocalKeyWrapper implements this with an env-provided keyring.
// In production, AWSKMSKeyWrapper can be plugged in with zero DB schema changes.
type KeyWrapper interface {
	ActiveKeyID() string
	Wrap(ctx context.Context, dek []byte) (keyID string, wrappedDEK []byte, err error)
	Unwrap(ctx context.Context, keyID string, wrappedDEK []byte) ([]byte, error)
}

// LocalKeyWrapper wraps DEKs using AES-256-GCM keys supplied via EUNOMIA_MASTER_KEYS.
// The first key in the keyring is active for writes; all keys remain usable for unwrapping
// so master key rotation can happen with zero downtime.
type LocalKeyWrapper struct {
	activeID string
	keys     map[string][]byte
}

// ParseLocalKeyWrapper parses a comma-separated keyring of "<key_id>:<base64-32-byte-key>".
// Example: "v2:<base64>,v1:<base64>".
func ParseLocalKeyWrapper(spec string) (*LocalKeyWrapper, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("master keyring is empty")
	}

	parts := strings.Split(spec, ",")
	keys := make(map[string][]byte, len(parts))
	var activeID string

	for i, raw := range parts {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("master keyring entry %d is empty", i)
		}
		id, b64Key, ok := strings.Cut(entry, ":")
		id = strings.TrimSpace(id)
		b64Key = strings.TrimSpace(b64Key)
		if !ok || id == "" || b64Key == "" {
			return nil, fmt.Errorf("master keyring entry %d must have format <key_id>:<base64>", i)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("duplicate master key id %q", id)
		}
		decoded, err := base64.StdEncoding.DecodeString(b64Key)
		if err != nil {
			return nil, fmt.Errorf("decoding master key %q: %w", id, err)
		}
		if len(decoded) != keyBytes {
			return nil, fmt.Errorf("master key %q must decode to %d bytes, got %d", id, keyBytes, len(decoded))
		}
		if activeID == "" {
			activeID = id
		}
		keys[id] = decoded
	}

	return &LocalKeyWrapper{
		activeID: activeID,
		keys:     keys,
	}, nil
}

// GenerateLocalKey generates a cryptographically random 32-byte key formatted as "<id>:<base64>".
func GenerateLocalKey(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("key id is required")
	}
	if strings.ContainsAny(id, ":, \t\r\n") {
		return "", fmt.Errorf("key id %q contains invalid characters", id)
	}
	buf := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("generating random key: %w", err)
	}
	return fmt.Sprintf("%s:%s", id, base64.StdEncoding.EncodeToString(buf)), nil
}

func (w *LocalKeyWrapper) ActiveKeyID() string {
	return w.activeID
}

func (w *LocalKeyWrapper) Wrap(_ context.Context, dek []byte) (string, []byte, error) {
	if len(dek) != keyBytes {
		return "", nil, fmt.Errorf("dek must be %d bytes, got %d", keyBytes, len(dek))
	}
	gcm := mustGCM(w.keys[w.activeID])
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", nil, fmt.Errorf("generating wrap nonce: %w", err)
	}
	// Format: nonce || ciphertext+tag, with keyID bound as AAD.
	sealed := gcm.Seal(nonce, nonce, dek, []byte(w.activeID))
	return w.activeID, sealed, nil
}

func (w *LocalKeyWrapper) Unwrap(_ context.Context, keyID string, wrappedDEK []byte) ([]byte, error) {
	kek, ok := w.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("unknown master key id %q", keyID)
	}
	gcm := mustGCM(kek)
	ns := gcm.NonceSize()
	if len(wrappedDEK) <= ns {
		return nil, errors.New("wrapped DEK is truncated")
	}
	nonce, ciphertext := wrappedDEK[:ns], wrappedDEK[ns:]
	dek, err := gcm.Open(nil, nonce, ciphertext, []byte(keyID))
	if err != nil {
		return nil, fmt.Errorf("unwrapping DEK with key %q: %w", keyID, err)
	}
	if len(dek) != keyBytes {
		return nil, fmt.Errorf("unwrapped DEK has unexpected length %d", len(dek))
	}
	return dek, nil
}

// mustGCM creates an AES-256-GCM AEAD from a pre-validated 32-byte key.
func mustGCM(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return gcm
}
