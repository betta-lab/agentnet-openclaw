package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/btcutil/base58"
)

// ── Canonical JSON ──────────────────────────────────────────────────────────

func TestCanonicalJSON_Ordering(t *testing.T) {
	msg := map[string]interface{}{
		"z": "last",
		"a": "first",
		"m": "mid",
	}
	canon, err := canonicalJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"a":"first","m":"mid","z":"last"}`
	if string(canon) != expected {
		t.Fatalf("got %s, want %s", string(canon), expected)
	}
}

func TestCanonicalJSON_Nested(t *testing.T) {
	msg := map[string]interface{}{
		"b": map[string]interface{}{"z": float64(1), "a": float64(2)},
		"a": "top",
	}
	canon, _ := canonicalJSON(msg)
	expected := `{"a":"top","b":{"a":2,"z":1}}`
	if string(canon) != expected {
		t.Fatalf("got %s, want %s", string(canon), expected)
	}
}

func TestCanonicalJSON_Arrays(t *testing.T) {
	msg := map[string]interface{}{
		"tags": []interface{}{"b", "a", "c"},
	}
	canon, _ := canonicalJSON(msg)
	expected := `{"tags":["b","a","c"]}`
	if string(canon) != expected {
		t.Fatalf("got %s, want %s", string(canon), expected)
	}
}

// ── Nonce / UUID ────────────────────────────────────────────────────────────

func TestRandomNonce_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		n := randomNonce()
		if seen[n] {
			t.Fatalf("duplicate nonce at iteration %d", i)
		}
		seen[n] = true
	}
}

func TestRandomNonce_NotEmpty(t *testing.T) {
	n := randomNonce()
	if len(n) == 0 {
		t.Fatal("nonce is empty")
	}
}

func TestRandomUUID_Format(t *testing.T) {
	uuid := randomUUID()
	if len(uuid) != 36 {
		t.Fatalf("UUID length: got %d, want 36", len(uuid))
	}
	if uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' {
		t.Fatalf("UUID format invalid: %s", uuid)
	}
	if uuid[14] != '4' {
		t.Fatalf("UUID version: got %c, want 4", uuid[14])
	}
}

func TestRandomUUID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		u := randomUUID()
		if seen[u] {
			t.Fatalf("duplicate UUID at iteration %d", i)
		}
		seen[u] = true
	}
}

// ── PoW Solver ──────────────────────────────────────────────────────────────

func TestSolvePoW_Valid(t *testing.T) {
	challenge := "test-challenge-abc"
	difficulty := 16

	proof := solvePoW(challenge, difficulty)

	h := sha256.New()
	h.Write([]byte(challenge))
	h.Write([]byte(proof))
	hash := h.Sum(nil)

	for i := 0; i < difficulty; i++ {
		byteIdx := i / 8
		bitIdx := uint(7 - (i % 8))
		if hash[byteIdx]&(1<<bitIdx) != 0 {
			t.Fatalf("PoW proof does not meet difficulty %d at bit %d", difficulty, i)
		}
	}
}

func TestSolvePoW_VariousDifficulties(t *testing.T) {
	for _, diff := range []int{4, 8, 12, 16} {
		proof := solvePoW("test", diff)
		if !verifyPoW("test", proof, diff) {
			t.Fatalf("proof failed at difficulty %d", diff)
		}
	}
}

func TestSolvePoW_DifferentChallenges(t *testing.T) {
	proof1 := solvePoW("challenge-1", 12)
	proof2 := solvePoW("challenge-2", 12)

	// Proof for one challenge shouldn't work for another (almost certainly)
	if verifyPoW("challenge-1", proof2, 12) && verifyPoW("challenge-2", proof1, 12) {
		t.Fatal("proofs should not be interchangeable between challenges")
	}
}

// ── Signing ─────────────────────────────────────────────────────────────────

func TestSign_ProducesValidSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	c := &Client{
		agentID: base58.Encode(pub),
		privKey: priv,
	}

	msg := map[string]interface{}{
		"type":      "hello",
		"timestamp": time.Now().UnixMilli(),
		"nonce":     randomNonce(),
	}
	sig := c.sign(msg)

	canon, _ := canonicalJSON(msg)
	sigBytes := base58.Decode(sig)
	if !ed25519.Verify(pub, canon, sigBytes) {
		t.Fatal("signature verification failed")
	}
}

func TestSign_TamperedMessageFails(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	c := &Client{
		agentID: base58.Encode(pub),
		privKey: priv,
	}

	msg := map[string]interface{}{
		"type": "hello",
		"data": "original",
	}
	sig := c.sign(msg)

	msg["data"] = "tampered"
	canon, _ := canonicalJSON(msg)
	sigBytes := base58.Decode(sig)
	if ed25519.Verify(pub, canon, sigBytes) {
		t.Fatal("tampered message should not verify")
	}
}

func TestSign_DifferentKeys(t *testing.T) {
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	c := &Client{
		agentID: base58.Encode(pub1),
		privKey: priv1,
	}

	msg := map[string]interface{}{"type": "test"}
	sig := c.sign(msg)

	// Verify with wrong key should fail
	canon, _ := canonicalJSON(msg)
	sigBytes := base58.Decode(sig)
	if ed25519.Verify(pub2, canon, sigBytes) {
		t.Fatal("wrong key should not verify")
	}
}

// ── Canonical JSON edge cases ───────────────────────────────────────────────

func TestCanonicalJSON_EmptyObject(t *testing.T) {
	canon, _ := canonicalJSON(map[string]interface{}{})
	if string(canon) != "{}" {
		t.Fatalf("got %s, want {}", string(canon))
	}
}

func TestCanonicalJSON_EmptyArray(t *testing.T) {
	msg := map[string]interface{}{
		"tags": []interface{}{},
	}
	canon, _ := canonicalJSON(msg)
	if string(canon) != `{"tags":[]}` {
		t.Fatalf("got %s", string(canon))
	}
}

func TestCanonicalJSON_NullValue(t *testing.T) {
	msg := map[string]interface{}{
		"key": nil,
	}
	canon, _ := canonicalJSON(msg)
	if string(canon) != `{"key":null}` {
		t.Fatalf("got %s", string(canon))
	}
}

func TestCanonicalJSON_BoolValues(t *testing.T) {
	msg := map[string]interface{}{
		"true":  true,
		"false": false,
	}
	canon, _ := canonicalJSON(msg)
	expected := `{"false":false,"true":true}`
	if string(canon) != expected {
		t.Fatalf("got %s, want %s", string(canon), expected)
	}
}

func TestCanonicalJSON_SignatureRemoved(t *testing.T) {
	// Ensure that signing works correctly: signature is not part of the signed data
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := &Client{agentID: base58.Encode(pub), privKey: priv}

	msg := map[string]interface{}{
		"type":  "test",
		"nonce": "abc",
	}
	sig := c.sign(msg) // signs without "signature" field

	// Adding signature to msg should not affect verification
	// because verification removes "signature" before hashing
	msg["signature"] = sig
	delete(msg, "signature")
	canon, _ := canonicalJSON(msg)
	sigBytes := base58.Decode(sig)
	if !ed25519.Verify(pub, canon, sigBytes) {
		t.Fatal("signature verification failed after roundtrip")
	}
}
