// SPDX-License-Identifier: Apache-2.0

package key

import (
	"bytes"
	"sync"
	"testing"
)

func testMaterial() (cryptKey, idKey []byte) {
	cryptKey = make([]byte, 64)
	idKey = make([]byte, 32)
	for i := range cryptKey {
		cryptKey[i] = byte(i + 1)
	}
	for i := range idKey {
		idKey[i] = byte(0xa0 + i)
	}
	return cryptKey, idKey
}

func aeadModes(t *testing.T) map[string]Key {
	t.Helper()
	cryptKey, idKey := testMaterial()
	out := map[string]Key{}
	for _, name := range []string{
		"aes256-ocb", "chacha20-poly1305", "blake3-aes256-ocb", "blake3-chacha20-poly1305",
	} {
		k, err := ByName(name, cryptKey, idKey)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = k
	}
	return out
}

func TestAEADRoundTrip(t *testing.T) {
	for name, k := range aeadModes(t) {
		t.Run(name, func(t *testing.T) {
			for _, data := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("borge "), 5000)} {
				id := k.IDHash(data)
				aad := []byte("BORG_OBJ")
				env, err := k.Encrypt(id, data, aad)
				if err != nil {
					t.Fatal(err)
				}
				if len(env) != aeadPayloadOverhead+len(data) {
					t.Errorf("envelope is %d bytes, want %d", len(env), aeadPayloadOverhead+len(data))
				}
				if env[0] != k.Type() {
					t.Errorf("type byte is 0x%02x, want 0x%02x", env[0], k.Type())
				}
				if bytes.Contains(env[aeadPayloadOverhead:], []byte("borge borge")) {
					t.Error("plaintext is visible in the envelope")
				}
				got, err := k.Decrypt(id, env, aad)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Errorf("round trip changed the data (%d vs %d bytes)", len(got), len(data))
				}
			}
		})
	}
}

// TestAEADRejectsTampering covers the three things the tag is supposed to bind: the
// payload, the header (including the chunk id in the AAD) and the AAD itself.
func TestAEADRejectsTampering(t *testing.T) {
	for name, k := range aeadModes(t) {
		t.Run(name, func(t *testing.T) {
			data := []byte("the quick brown fox")
			id := k.IDHash(data)
			aad := []byte("BORG_OBJ")
			env, err := k.Encrypt(id, data, aad)
			if err != nil {
				t.Fatal(err)
			}

			cases := map[string]func() ([]byte, []byte, []byte){
				"flipped ciphertext bit": func() ([]byte, []byte, []byte) {
					bad := append([]byte(nil), env...)
					bad[len(bad)-1] ^= 1
					return id, bad, aad
				},
				"flipped tag bit": func() ([]byte, []byte, []byte) {
					bad := append([]byte(nil), env...)
					bad[aeadHeaderSize] ^= 1
					return id, bad, aad
				},
				"altered session id": func() ([]byte, []byte, []byte) {
					bad := append([]byte(nil), env...)
					bad[aeadSessionOffset] ^= 1
					return id, bad, aad
				},
				"altered message counter": func() ([]byte, []byte, []byte) {
					bad := append([]byte(nil), env...)
					bad[aeadIVOffset+aeadIVSize-1] ^= 1
					return id, bad, aad
				},
				"wrong chunk id": func() ([]byte, []byte, []byte) {
					other := append([]byte(nil), id...)
					other[0] ^= 1
					return other, env, aad
				},
				"wrong aad": func() ([]byte, []byte, []byte) {
					return id, env, []byte("BORG_XXX")
				},
				"truncated": func() ([]byte, []byte, []byte) {
					return id, env[:aeadPayloadOverhead-1], aad
				},
			}
			for what, mangle := range cases {
				gotID, gotEnv, gotAAD := mangle()
				if _, err := k.Decrypt(gotID, gotEnv, gotAAD); err == nil {
					t.Errorf("%s was accepted", what)
				}
			}
		})
	}
}

// TestAEADCounterIsUnique is the property the whole session design exists for: no
// (session id, message counter) pair may ever be handed out twice.
func TestAEADCounterIsUnique(t *testing.T) {
	cryptKey, idKey := testMaterial()
	k, err := NewAESOCB(cryptKey, idKey)
	if err != nil {
		t.Fatal(err)
	}

	const goroutines, each = 8, 200
	var mu sync.Mutex
	seen := map[string]bool{}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				env, err := k.Encrypt(make([]byte, 32), []byte("payload"), nil)
				if err != nil {
					t.Error(err)
					return
				}
				nonce := string(env[aeadIVOffset : aeadSessionOffset+aeadSessionIDSize])
				mu.Lock()
				if seen[nonce] {
					t.Error("a (session id, counter) pair was handed out twice")
				}
				seen[nonce] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*each {
		t.Errorf("got %d distinct nonces, want %d", len(seen), goroutines*each)
	}
}

// TestAEADFirstCounterIsOne pins borg's off-by-one: the cipher is built at IV 0 and
// next_iv() pre-increments, so the first message on the wire carries 1.
func TestAEADFirstCounterIsOne(t *testing.T) {
	cryptKey, idKey := testMaterial()
	k, err := NewCHPO(cryptKey, idKey)
	if err != nil {
		t.Fatal(err)
	}
	for want := uint64(1); want <= 3; want++ {
		env, err := k.Encrypt(make([]byte, 32), []byte("x"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := uint48(env[aeadIVOffset : aeadIVOffset+aeadIVSize]); got != want {
			t.Errorf("message counter is %d, want %d", got, want)
		}
	}
}

// TestAEADSessionsAreIndependent: an envelope stays readable after the writer has rolled
// over to a new session, because the session id travels with it.
func TestAEADSessionsAreIndependent(t *testing.T) {
	cryptKey, idKey := testMaterial()
	k, err := NewAESOCB(cryptKey, idKey)
	if err != nil {
		t.Fatal(err)
	}
	ak := k.(*aeadKey)

	data := []byte("written in the first session")
	id := k.IDHash(data)
	env, err := k.Encrypt(id, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := ak.SessionID()

	ak.mu.Lock()
	err = ak.newSession()
	ak.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, ak.SessionID()) {
		t.Fatal("the new session reused the old session id")
	}

	got, err := k.Decrypt(id, env, nil)
	if err != nil {
		t.Fatalf("an object from the previous session became unreadable: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("the data changed across a session roll-over")
	}
}

// TestAEADIDHashDiffersPerMode: the two id-hash families must not agree, or a repository
// created in one mode would appear to deduplicate against the other.
func TestAEADIDHashDiffersPerMode(t *testing.T) {
	modes := aeadModes(t)
	data := []byte("some chunk")
	sha := modes["aes256-ocb"].IDHash(data)
	if !bytes.Equal(sha, modes["chacha20-poly1305"].IDHash(data)) {
		t.Error("the two sha256 modes disagree on chunk ids")
	}
	b3 := modes["blake3-aes256-ocb"].IDHash(data)
	if !bytes.Equal(b3, modes["blake3-chacha20-poly1305"].IDHash(data)) {
		t.Error("the two blake3 modes disagree on chunk ids")
	}
	if bytes.Equal(sha, b3) {
		t.Error("the sha256 and blake3 id hashes agree, which cannot be right")
	}
}

func TestAEADRejectsWrongKeySizes(t *testing.T) {
	if _, err := NewAESOCB(make([]byte, 32), make([]byte, 32)); err == nil {
		t.Error("a 32 byte crypt key was accepted")
	}
	if _, err := NewAESOCB(make([]byte, 64), make([]byte, 16)); err == nil {
		t.Error("a 16 byte id key was accepted")
	}
}
