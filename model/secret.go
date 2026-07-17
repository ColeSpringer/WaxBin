package model

// SecretCipher seals and opens the values WaxBin stores in its secret table
// (today, private-feed passwords). An embedder such as WaxDeck supplies one to
// own the key material and encrypt those values at rest; when none is supplied
// the standalone CLI keeps secrets in plaintext.
//
// Both methods take associated data (aad). WaxBin passes the secret's key (for
// example "podcast.auth.<pid>") as aad, binding each ciphertext to the row it
// belongs to: a sealed value lifted into another secret's row fails to open,
// because the aad no longer matches. The aad is authenticated, not encrypted, so
// an implementation must reject an open whose aad differs from the seal's.
//
// The interface is deliberately minimal and carries associated data from the
// start: it is a cross-repo contract, and widening it after an embedder ships
// against a narrower shape would be a breaking change.
type SecretCipher interface {
	// Seal encrypts plaintext, authenticating aad, and returns the ciphertext.
	Seal(aad, plaintext []byte) (ciphertext []byte, err error)
	// Open decrypts ciphertext, verifying aad, and returns the plaintext. It must
	// fail when aad does not match the value passed to Seal.
	Open(aad, ciphertext []byte) (plaintext []byte, err error)
}
