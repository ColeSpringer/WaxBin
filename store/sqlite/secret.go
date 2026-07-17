package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// A sealed secret value is stored as "wbenc:v1:<keyID>:<base64(ciphertext)>". The
// wbenc prefix marks a value as sealed (so a plaintext value can never be mistaken
// for one), v1 is the envelope version, and keyID names the key/epoch that sealed
// it so a rotation is tellable apart from the prior generation.
const (
	sealPrefix         = "wbenc:"
	sealVersion        = "v1"
	defaultSecretKeyID = "1"
)

// looksSealed reports whether a stored value carries the sealed-value marker.
func looksSealed(v string) bool { return strings.HasPrefix(v, sealPrefix) }

// validateSecretKeyID rejects a key id that would corrupt the colon-delimited wbenc
// envelope. It must be non-empty and contain no ':', so an embedder's namespaced label
// (e.g. "prod:1") is caught at configuration time rather than silently bricking every
// GetSecret when the base64 fails to split back out.
func validateSecretKeyID(keyID string) error {
	const op = "store.secret"
	if keyID == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "secret key id must not be empty")
	}
	if strings.ContainsRune(keyID, ':') {
		return waxerr.New(waxerr.CodeInvalid, op, "secret key id must not contain ':': "+keyID)
	}
	return nil
}

// sealValue seals plaintext for the secret named key (used as associated data)
// under keyID, producing the wbenc envelope.
func sealValue(cipher model.SecretCipher, keyID, key, plaintext string) (string, error) {
	ct, err := cipher.Seal([]byte(key), []byte(plaintext))
	if err != nil {
		return "", err
	}
	return sealPrefix + sealVersion + ":" + keyID + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

// openValue reverses sealValue for the secret named key, verifying that key
// matches the associated data the value was sealed with. It rejects a value whose
// envelope version is not understood.
func openValue(cipher model.SecretCipher, key, stored string) (string, error) {
	const op = "store.openSecret"
	rest := strings.TrimPrefix(stored, sealPrefix)
	// rest is "<version>:<keyID>:<base64>"; split off version and keyID, keeping the
	// base64 (which never contains ':') intact.
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return "", waxerr.New(waxerr.CodeInvalid, op, "malformed sealed secret")
	}
	if parts[0] != sealVersion {
		return "", waxerr.New(waxerr.CodeInvalid, op, "unsupported sealed-secret version: "+parts[0])
	}
	keyID := parts[1]
	if keyID == "" {
		return "", waxerr.New(waxerr.CodeInvalid, op, "sealed secret has no key id")
	}
	ct, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeInvalid, op, err)
	}
	pt, err := cipher.Open([]byte(key), ct)
	if err != nil {
		// Name the key id the value was sealed under, so a cipher/epoch mismatch after a
		// rotation is diagnosable rather than an opaque authentication failure.
		return "", waxerr.Wrapf(waxerr.CodeInvalid, op, err, "opening secret sealed under key %q", keyID)
	}
	return string(pt), nil
}

// ReSealSecrets seals every plaintext secret with the store's configured cipher,
// leaving already-sealed values untouched, in one transaction. It is the adoption
// path a catalog runs once after a cipher is first configured; it is idempotent, so
// running it again is a no-op. It requires a configured cipher.
func (s *Store) ReSealSecrets(ctx context.Context) (int, error) {
	const op = "store.ReSealSecrets"
	if s.cipher == nil {
		return 0, waxerr.New(waxerr.CodeUnsupported, op, "no secret cipher is configured")
	}
	var n int
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		rows, err := readAllSecretsTx(ctx, tx)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, r := range rows {
			if looksSealed(r.value) {
				continue
			}
			sealed, err := sealValue(s.cipher, s.cipherKeyID, r.key, r.value)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeInternal, op, err)
			}
			if err := updateSecretValueTx(ctx, tx, r.key, sealed); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			n++
		}
		return nil
	})
	return n, err
}

// RotateSecrets re-seals every sealed secret from oldCipher to newCipher under
// newKeyID, in one transaction, so a crash rolls the whole rotation back rather
// than leaving a mix of old- and new-key values. A value not yet sealed is sealed
// with newCipher (adoption folds into rotation). After a successful rotation the
// caller reopens the store with newCipher as its configured cipher.
func (s *Store) RotateSecrets(ctx context.Context, oldCipher, newCipher model.SecretCipher, newKeyID string) (int, error) {
	const op = "store.RotateSecrets"
	if oldCipher == nil || newCipher == nil {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "rotation needs both an old and a new cipher")
	}
	if newKeyID == "" {
		newKeyID = defaultSecretKeyID
	}
	if err := validateSecretKeyID(newKeyID); err != nil {
		return 0, err
	}
	var n int
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		rows, err := readAllSecretsTx(ctx, tx)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, r := range rows {
			plaintext := r.value
			if looksSealed(r.value) {
				pt, err := openValue(oldCipher, r.key, r.value)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeInvalid, op, err, "opening secret %s with the old cipher", r.key)
				}
				plaintext = pt
			}
			sealed, err := sealValue(newCipher, newKeyID, r.key, plaintext)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeInternal, op, err)
			}
			if err := updateSecretValueTx(ctx, tx, r.key, sealed); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			n++
		}
		return nil
	})
	return n, err
}

type secretRow struct {
	key   string
	value string
}

// readAllSecretsTx drains every secret row, closing its cursor before returning so
// the caller can write to the same single-connection transaction.
func readAllSecretsTx(ctx context.Context, tx *sql.Tx) ([]secretRow, error) {
	rows, err := tx.QueryContext(ctx, "SELECT key, value FROM secret ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []secretRow
	for rows.Next() {
		var r secretRow
		if err := rows.Scan(&r.key, &r.value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func updateSecretValueTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, "UPDATE secret SET value=?, updated_at=? WHERE key=?", value, nowNS(), key)
	return err
}

// restrictSecretFiles chmods the files that can carry a plaintext secret to
// owner-only (0600). It runs on a read-write open and after a backup, and warns
// (never fails) when the filesystem does not support Unix permissions, matching the
// existing lockfile/socket handling. A file that does not exist yet (a WAL sidecar
// before the first checkpoint) is skipped silently.
func (s *Store) restrictSecretFiles(paths ...string) {
	if len(paths) == 0 {
		paths = []string{s.path, s.path + "-wal", s.path + "-shm"}
	}
	chmodSecretFiles(s.log.Warn, paths...)
}

// chmodSecretFiles best-effort restricts each path to 0600, reporting a warning
// through warnf for anything but a missing file.
func chmodSecretFiles(warnf func(msg string, args ...any), paths ...string) {
	for _, p := range paths {
		if err := os.Chmod(p, 0o600); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			warnf("could not restrict secret-bearing file permissions", "path", p, "err", err)
		}
	}
}
