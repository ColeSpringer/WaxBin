package sqlite_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

// aeadCipher is a test-only reversible SecretCipher backed by AES-GCM. WaxBin ships
// no cipher; an embedder supplies one. The nonce is a fixed all-zero value so a seal
// is deterministic for the test, which is fine because each test uses its own key.
type aeadCipher struct{ aead cipher.AEAD }

func newAEAD(t *testing.T, key string) *aeadCipher {
	t.Helper()
	k := make([]byte, 32)
	copy(k, key)
	block, err := aes.NewCipher(k)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	return &aeadCipher{aead: a}
}

func (c *aeadCipher) Seal(aad, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	return c.aead.Seal(nonce, nonce, plaintext, aad), nil
}

func (c *aeadCipher) Open(aad, ct []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(ct) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return c.aead.Open(nil, ct[:ns], ct[ns:], aad)
}

func openCipheredStore(t *testing.T, dir, keyID string, c *aeadCipher) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{
		Path: filepath.Join(dir, "catalog.db"), Owner: "test",
		SecretCipher: c, SecretKeyID: keyID,
	})
	if err != nil {
		t.Fatalf("open ciphered store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSecretSealRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newAEAD(t, "key-a")
	st := openCipheredStore(t, dir, "1", c)

	if err := st.SetSecret(ctx, "podcast.auth.p1", "hunter2"); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	got, err := st.GetSecret(ctx, "podcast.auth.p1")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("round-trip = %q, want hunter2", got)
	}

	// The value is sealed at rest: a plaintext-mode reader must not see the secret.
	raw := rawSecret(t, dir, "podcast.auth.p1")
	if raw == "hunter2" || len(raw) == 0 {
		t.Fatalf("stored value is not sealed: %q", raw)
	}
	if raw[:6] != "wbenc:" {
		t.Fatalf("stored value missing seal marker: %q", raw)
	}
}

func TestSecretWrongAADFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newAEAD(t, "key-a")
	st := openCipheredStore(t, dir, "1", c)

	if err := st.SetSecret(ctx, "podcast.auth.p1", "hunter2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Move the sealed bytes onto a different key (a different AAD) and confirm the
	// open fails, since a sealed value cannot be lifted into another secret's row.
	raw := rawSecret(t, dir, "podcast.auth.p1")
	setRawSecret(t, dir, "podcast.auth.p2", raw)
	if _, err := st.GetSecret(ctx, "podcast.auth.p2"); err == nil {
		t.Fatal("expected wrong-AAD open to fail")
	}
}

func TestSecretPlaintextWhenNoCipher(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(dir, "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.SetSecret(ctx, "podcast.auth.p1", "hunter2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if raw := rawSecret(t, dir, "podcast.auth.p1"); raw != "hunter2" {
		t.Fatalf("plaintext store = %q, want hunter2", raw)
	}
	// A plaintext value beginning with the reserved marker is refused.
	if err := st.SetSecret(ctx, "k", "wbenc:evil"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("expected CodeInvalid for marker-prefixed value, got %v", err)
	}
}

func TestSecretSealedWithoutCipherErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Seal a value with a cipher, then reopen with no cipher and confirm the read
	// errors rather than returning ciphertext.
	c := newAEAD(t, "key-a")
	st := openCipheredStore(t, dir, "1", c)
	if err := st.SetSecret(ctx, "k", "secret"); err != nil {
		t.Fatalf("set: %v", err)
	}
	_ = st.Close()

	plain, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(dir, "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if _, err := plain.GetSecret(ctx, "k"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("expected CodeInvalid reading a sealed value without a cipher, got %v", err)
	}
}

func TestReSealSecretsAdoption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Write a plaintext secret, then adopt it under a cipher.
	plain, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(dir, "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open plain: %v", err)
	}
	if err := plain.SetSecret(ctx, "k", "hunter2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	_ = plain.Close()

	c := newAEAD(t, "key-a")
	st := openCipheredStore(t, dir, "1", c)
	n, err := st.ReSealSecrets(ctx)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if n != 1 {
		t.Fatalf("resealed %d, want 1", n)
	}
	if raw := rawSecret(t, dir, "k"); len(raw) < 6 || raw[:6] != "wbenc:" {
		t.Fatalf("secret not sealed after adoption: %q", raw)
	}
	if got, err := st.GetSecret(ctx, "k"); err != nil || got != "hunter2" {
		t.Fatalf("read after adoption = %q, %v", got, err)
	}
	// Idempotent: a second run seals nothing.
	if n, err := st.ReSealSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("second reseal = %d, %v; want 0, nil", n, err)
	}
}

func TestRotateSecretsEpochTellsRowsApart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	oldC := newAEAD(t, "key-old")
	newC := newAEAD(t, "key-new")

	st := openCipheredStore(t, dir, "1", oldC)
	if err := st.SetSecret(ctx, "k", "hunter2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if raw := rawSecret(t, dir, "k"); raw[:9] != "wbenc:v1:" || raw[9:11] != "1:" {
		t.Fatalf("pre-rotation keyID not 1: %q", raw)
	}

	n, err := st.RotateSecrets(ctx, oldC, newC, "2")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if n != 1 {
		t.Fatalf("rotated %d, want 1", n)
	}
	// The rotated row now carries the new epoch label, so it is tellable apart.
	if raw := rawSecret(t, dir, "k"); raw[9:11] != "2:" {
		t.Fatalf("post-rotation keyID not 2: %q", raw)
	}
	_ = st.Close()

	// Reopen with the new cipher and confirm the value opens.
	st2 := openCipheredStore(t, dir, "2", newC)
	if got, err := st2.GetSecret(ctx, "k"); err != nil || got != "hunter2" {
		t.Fatalf("read after rotation = %q, %v", got, err)
	}
}

func TestSecretKeyIDWithColonRejected(t *testing.T) {
	ctx := context.Background()
	c := newAEAD(t, "key-a")
	// A ':' in the key id would corrupt the colon-delimited envelope, so Open rejects it.
	_, err := sqlite.Open(ctx, sqlite.OpenOptions{
		Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test",
		SecretCipher: c, SecretKeyID: "prod:1",
	})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("open with colon key id = %v, want CodeInvalid", err)
	}
	// A clean key id opens fine and round-trips.
	dir := t.TempDir()
	st := openCipheredStore(t, dir, "prod-1", c)
	if err := st.SetSecret(ctx, "k", "v"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := st.GetSecret(ctx, "k"); err != nil || got != "v" {
		t.Fatalf("round-trip = %q, %v", got, err)
	}
	// RotateSecrets rejects a colon key id too.
	if _, err := st.RotateSecrets(ctx, c, c, "new:2"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("rotate with colon key id = %v, want CodeInvalid", err)
	}
}

func TestSecretFilePerms0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: dbPath, Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSecret(ctx, "k", "v"); err != nil {
		t.Fatalf("set: %v", err)
	}
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("catalog perms = %o, want 600", perm)
	}

	// A backup carries the secret table and must also be restricted.
	backup := filepath.Join(dir, "backup.db")
	if err := st.BackupTo(ctx, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	bi, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := bi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("backup perms = %o, want 600", perm)
	}
}

// rawSecret reads the on-disk secret value directly (bypassing the cipher) via a
// separate read-only connection, to assert what is actually persisted.
func rawSecret(t *testing.T, dir, key string) string {
	t.Helper()
	db := openRawDB(t, dir)
	var v string
	if err := db.QueryRow("SELECT value FROM secret WHERE key=?", key).Scan(&v); err != nil {
		t.Fatalf("raw read %s: %v", key, err)
	}
	return v
}

func setRawSecret(t *testing.T, dir, key, value string) {
	t.Helper()
	db := openRawDB(t, dir)
	if _, err := db.Exec("INSERT INTO secret(key,value,updated_at) VALUES(?,?,0)", key, value); err != nil {
		t.Fatalf("raw write %s: %v", key, err)
	}
}

// openRawDB opens a direct database/sql handle to the catalog, bypassing the store,
// with a busy timeout so a brief write does not race the store's idle write lock.
func openRawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "catalog.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
