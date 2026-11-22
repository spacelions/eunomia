package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

type tenantSecretRow struct {
	bun.BaseModel `bun:"table:tenant_secrets,alias:ts"`

	TenantID      string    `bun:"tenant_id,pk"`
	ConnectorType string    `bun:"connector_type,pk"`
	SecretKey     string    `bun:"secret_key,pk"`
	KeyID         string    `bun:"key_id,notnull"`
	EncryptedDEK  []byte    `bun:"encrypted_dek,notnull"`
	Nonce         []byte    `bun:"nonce,notnull"`
	Ciphertext    []byte    `bun:"ciphertext,notnull"`
	CreatedAt     time.Time `bun:"created_at,notnull"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"`
}

// DBSecretProvider stores envelope-encrypted secrets in PostgreSQL (tenant_secrets).
type DBSecretProvider struct {
	db      *bun.DB
	wrapper KeyWrapper
}

func NewDBSecretProvider(db *bun.DB, wrapper KeyWrapper) *DBSecretProvider {
	return &DBSecretProvider{db: db, wrapper: wrapper}
}

func (p *DBSecretProvider) Set(ctx context.Context, tenantID, connector, key string, value []byte) error {
	env, err := EncryptEnvelope(ctx, p.wrapper, tenantID, connector, key, value)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	row := &tenantSecretRow{
		TenantID:      tenantID,
		ConnectorType: connector,
		SecretKey:     key,
		KeyID:         env.KeyID,
		EncryptedDEK:  env.EncryptedDEK,
		Nonce:         env.Nonce,
		Ciphertext:    env.Ciphertext,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	_, err = p.db.NewInsert().
		Model(row).
		On("CONFLICT (tenant_id, connector_type, secret_key) DO UPDATE").
		Set("key_id = EXCLUDED.key_id").
		Set("encrypted_dek = EXCLUDED.encrypted_dek").
		Set("nonce = EXCLUDED.nonce").
		Set("ciphertext = EXCLUDED.ciphertext").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upserting secret %s/%s/%s: %w", tenantID, connector, key, err)
	}
	return nil
}

func (p *DBSecretProvider) Get(ctx context.Context, tenantID, connector, key string) ([]byte, error) {
	if err := validateCoordinates(tenantID, connector, key); err != nil {
		return nil, err
	}

	row := new(tenantSecretRow)
	err := p.db.NewSelect().
		Model(row).
		Where("tenant_id = ? AND connector_type = ? AND secret_key = ?", tenantID, connector, key).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s/%s/%s", ErrSecretNotFound, tenantID, connector, key)
		}
		return nil, fmt.Errorf("selecting secret %s/%s/%s: %w", tenantID, connector, key, err)
	}

	return DecryptEnvelope(ctx, p.wrapper, tenantID, connector, key, Envelope{
		KeyID:        row.KeyID,
		EncryptedDEK: row.EncryptedDEK,
		Nonce:        row.Nonce,
		Ciphertext:   row.Ciphertext,
	})
}

func (p *DBSecretProvider) List(ctx context.Context, tenantID string) ([]SecretMetadata, error) {
	if tenantID == "" {
		return nil, errors.New("tenant_id is required")
	}

	var rows []tenantSecretRow
	err := p.db.NewSelect().
		Model(&rows).
		Column("tenant_id", "connector_type", "secret_key", "key_id", "created_at", "updated_at").
		Where("tenant_id = ?", tenantID).
		OrderExpr("connector_type ASC, secret_key ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing secrets for tenant %q: %w", tenantID, err)
	}

	out := make([]SecretMetadata, 0, len(rows))
	for _, r := range rows {
		out = append(out, SecretMetadata{
			TenantID:  r.TenantID,
			Connector: r.ConnectorType,
			Key:       r.SecretKey,
			KeyID:     r.KeyID,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

func (p *DBSecretProvider) Delete(ctx context.Context, tenantID, connector, key string) error {
	if err := validateCoordinates(tenantID, connector, key); err != nil {
		return err
	}

	res, err := p.db.NewDelete().
		Model((*tenantSecretRow)(nil)).
		Where("tenant_id = ? AND connector_type = ? AND secret_key = ?", tenantID, connector, key).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting secret %s/%s/%s: %w", tenantID, connector, key, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s/%s/%s", ErrSecretNotFound, tenantID, connector, key)
	}
	return nil
}

// RotateMasterKey re-wraps the 32-byte DEK of every stored secret whose key_id differs
// from the KeyWrapper's active key ID. Ciphertext payloads are untouched.
func (p *DBSecretProvider) RotateMasterKey(ctx context.Context) (int, error) {
	activeKeyID := p.wrapper.ActiveKeyID()
	rotated := 0

	err := p.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var rows []tenantSecretRow
		if err := tx.NewSelect().
			Model(&rows).
			Column("tenant_id", "connector_type", "secret_key", "key_id", "encrypted_dek").
			Where("key_id <> ?", activeKeyID).
			For("UPDATE").
			Scan(ctx); err != nil {
			return fmt.Errorf("querying secrets to rotate: %w", err)
		}

		now := time.Now().UTC()
		for _, r := range rows {
			dek, err := p.wrapper.Unwrap(ctx, r.KeyID, r.EncryptedDEK)
			if err != nil {
				return fmt.Errorf("unwrapping %s/%s/%s: %w", r.TenantID, r.ConnectorType, r.SecretKey, err)
			}
			newKeyID, newWrappedDEK, wrapErr := p.wrapper.Wrap(ctx, dek)
			zeroBytes(dek)
			if wrapErr != nil {
				return fmt.Errorf("re-wrapping %s/%s/%s: %w", r.TenantID, r.ConnectorType, r.SecretKey, wrapErr)
			}

			_, err = tx.NewUpdate().
				Model((*tenantSecretRow)(nil)).
				Set("key_id = ?", newKeyID).
				Set("encrypted_dek = ?", newWrappedDEK).
				Set("updated_at = ?", now).
				Where("tenant_id = ? AND connector_type = ? AND secret_key = ?", r.TenantID, r.ConnectorType, r.SecretKey).
				Exec(ctx)
			if err != nil {
				return fmt.Errorf("updating rotated DEK for %s/%s/%s: %w", r.TenantID, r.ConnectorType, r.SecretKey, err)
			}
			rotated++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rotated, nil
}
