package keystore

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate_NewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")

	keys, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if len(keys.PublicKey) != ed25519.PublicKeySize {
		t.Fatalf("public key size: got %d, want %d", len(keys.PublicKey), ed25519.PublicKeySize)
	}
	if len(keys.PrivateKey) != ed25519.PrivateKeySize {
		t.Fatalf("private key size: got %d, want %d", len(keys.PrivateKey), ed25519.PrivateKeySize)
	}

	// File should exist with 0600 permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key file permissions: got %o, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreate_ExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")

	// Create key
	keys1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}

	// Load same key
	keys2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}

	if keys1.AgentID() != keys2.AgentID() {
		t.Fatalf("agent ID changed on reload: %s vs %s", keys1.AgentID(), keys2.AgentID())
	}

	// Verify signing still works
	msg := []byte("test message")
	sig := ed25519.Sign(keys2.PrivateKey, msg)
	if !ed25519.Verify(keys2.PublicKey, msg, sig) {
		t.Fatal("reloaded key cannot verify signatures")
	}
}

func TestLoadOrCreate_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "agent.key")

	_, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("should create nested directories: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("key file not found: %v", err)
	}
}

func TestAgentID_Base58Encoded(t *testing.T) {
	dir := t.TempDir()
	keys, _ := LoadOrCreate(filepath.Join(dir, "agent.key"))

	id := keys.AgentID()
	if len(id) == 0 {
		t.Fatal("agent ID is empty")
	}

	// Base58 characters only
	for _, c := range id {
		if !((c >= '1' && c <= '9') || (c >= 'A' && c <= 'H') || (c >= 'J' && c <= 'N') ||
			(c >= 'P' && c <= 'Z') || (c >= 'a' && c <= 'k') || (c >= 'm' && c <= 'z')) {
			t.Fatalf("agent ID contains non-base58 character: %c", c)
		}
	}
}

func TestLoadOrCreate_CorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")

	// Write garbage
	os.WriteFile(path, []byte("not json"), 0600)

	_, err := LoadOrCreate(path)
	if err == nil {
		t.Fatal("should error on corrupted key file")
	}
}
