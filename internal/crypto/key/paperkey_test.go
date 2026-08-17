// SPDX-License-Identifier: Apache-2.0

package key

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func testPaperBlob(t *testing.T) (Blob, string) {
	t.Helper()
	t.Setenv("BORGE_TESTONLY_WEAKEN_KDF", "1")
	repoID := testRepoID()
	repoIDHex := hex.EncodeToString(repoID)
	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	text, err := SealMaterial(material, "pass", AdminLabel)
	if err != nil {
		t.Fatal(err)
	}
	return Blob{ID: BlobName([]byte(text)), Text: []byte(text), Label: AdminLabel}, repoIDHex
}

func TestPaperKeyRoundTrip(t *testing.T) {
	blob, repoIDHex := testPaperBlob(t)

	paper, err := ExportPaperKey(blob, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(paper, paperKeyMagic) {
		t.Error("the export is missing its magic line")
	}
	if !strings.Contains(paper, "id: ") {
		t.Error("the export is missing its id line")
	}

	back, err := ImportPaperKey(paper, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	// The blob may be re-wrapped, so compare what it decodes to rather than the text.
	wantEnv, err := ParseEnvelope(blob.Text, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	gotEnv, err := ParseEnvelope(back, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotEnv.Data, wantEnv.Data) || !bytes.Equal(gotEnv.Salt, wantEnv.Salt) {
		t.Error("the paper key did not round trip")
	}
	if gotEnv.Label != AdminLabel {
		t.Errorf("the label was lost: %q", gotEnv.Label)
	}

	// And the reconstructed blob still unlocks.
	material, _, err := OpenMaterial(back, repoIDHex, "pass")
	if err != nil {
		t.Fatalf("the reconstructed key does not unlock: %v", err)
	}
	if len(material.CryptKey) != 64 {
		t.Error("the reconstructed material is the wrong shape")
	}
}

// TestPaperKeyCatchesTypos is the whole reason for the checksums: a mistyped digit must
// be reported with its line number, not discovered years later as an unusable backup.
func TestPaperKeyCatchesTypos(t *testing.T) {
	blob, repoIDHex := testPaperBlob(t)
	paper, err := ExportPaperKey(blob, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(paper, "\n"), "\n")

	// Find the second numbered line and change one hex digit in it.
	var target int
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "2:") {
			target = i
			break
		}
	}
	if target == 0 {
		t.Fatal("no line 2 in the export")
	}
	mangled := append([]string(nil), lines...)
	mangled[target] = strings.Replace(mangled[target], "a", "b", 1)
	if mangled[target] == lines[target] {
		mangled[target] = strings.Replace(mangled[target], "0", "1", 1)
	}

	_, err = ImportPaperKey(strings.Join(mangled, "\n"), repoIDHex)
	if err == nil {
		t.Fatal("a mistyped line was accepted")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("the error does not name the bad line: %v", err)
	}
}

func TestPaperKeyRejectsMissingLine(t *testing.T) {
	blob, repoIDHex := testPaperBlob(t)
	paper, err := ExportPaperKey(blob, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(paper, "\n"), "\n")
	var kept []string
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "3:") {
			continue
		}
		kept = append(kept, l)
	}
	if _, err := ImportPaperKey(strings.Join(kept, "\n"), repoIDHex); err == nil {
		t.Fatal("a paper key with a line missing was accepted")
	}
}

func TestPaperKeyRejectsAnotherRepository(t *testing.T) {
	blob, repoIDHex := testPaperBlob(t)
	paper, err := ExportPaperKey(blob, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("ab", 32)
	if _, err := ImportPaperKey(paper, other); err == nil {
		t.Fatal("a paper key for another repository was accepted")
	}
}

// TestPaperKeyGroupsDigits pins the printed layout: six characters per group is what
// makes it typeable, and it is part of what the checksums are computed against.
func TestPaperKeyGroupsDigits(t *testing.T) {
	if got := grouped("0123456789abcd"); got != "012345 6789ab cd" {
		t.Errorf("grouped() gave %q", got)
	}
	if got := grouped(""); got != "" {
		t.Errorf("grouped(\"\") gave %q", got)
	}
}

func TestPaperKeyHTMLCarriesTheKey(t *testing.T) {
	blob, _ := testPaperBlob(t)
	html, err := ExportPaperKeyHTML(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(html, blob.Text) {
		t.Error("the template does not carry the key")
	}
	if bytes.Count(html, paperKeyHTMLAnchor) != 1 {
		t.Error("the key was spliced in more than once")
	}
	// The template must stay self-contained: a printable key that fetches anything from
	// the network is a key handed to whoever answers.
	for _, forbidden := range []string{"http://", "https://", "//cdn."} {
		if bytes.Contains(html, []byte("src=\""+forbidden)) {
			t.Errorf("the template loads something external (%s)", forbidden)
		}
	}
}
