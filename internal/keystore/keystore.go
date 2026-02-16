package keystore

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcutil/base58"
)

// Keys holds an Ed25519 keypair.
type Keys struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// AgentID returns the base58-encoded public key.
func (k *Keys) AgentID() string {
	return base58.Encode(k.PublicKey)
}

type storedKey struct {
	PrivateKey string `json:"private_key"`
}

// LoadOrCreate loads keys from file, or creates a new keypair.
func LoadOrCreate(path string) (*Keys, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var sk storedKey
		if err := json.Unmarshal(data, &sk); err != nil {
			return nil, err
		}
		privBytes := base58.Decode(sk.PrivateKey)
		priv := ed25519.PrivateKey(privBytes)
		pub := priv.Public().(ed25519.PublicKey)
		return &Keys{PublicKey: pub, PrivateKey: priv}, nil
	}

	// Generate new keypair
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	sk := storedKey{PrivateKey: base58.Encode(priv)}
	data, _ = json.MarshalIndent(sk, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, err
	}

	return &Keys{PublicKey: pub, PrivateKey: priv}, nil
}
