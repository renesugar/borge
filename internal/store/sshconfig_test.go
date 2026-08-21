// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ssh config reader, against paramiko - which is what borg reads it with.
//
// The oracle here is deliberately not OpenSSH. borg reaches an sftp:// repository through
// borgstore, which reaches it through paramiko, so paramiko's answer is the one that
// decides where borg connects. Where paramiko and OpenSSH differ, following OpenSSH would
// make borge connect somewhere borg does not.

// paramikoLookup asks paramiko what a config file says about a host.
func paramikoLookup(t *testing.T, configPath, host string) map[string]any {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the ssh config differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the ssh config differential test")
	}
	const script = `
import json, sys
import paramiko
config = paramiko.SSHConfig.from_path(sys.argv[1])
found = config.lookup(sys.argv[2])
print(json.dumps({
    "hostname": found.get("hostname"),
    "user": found.get("user"),
    "port": found.get("port"),
    "identityfile": found.get("identityfile"),
}))
`
	out, err := exec.Command(py, "-c", script, configPath, host).Output()
	if err != nil {
		t.Fatalf("paramiko could not read %s: %v", configPath, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("paramiko's answer is not JSON: %v\n%s", err, out)
	}
	return parsed
}

// TestSSHConfigMatchesParamiko over the shapes a real config has.
func TestSSHConfigMatchesParamiko(t *testing.T) {
	config := `
# A comment, and a blank line follow.

Host plain
    HostName plain.example.com
    User alice
    Port 2222
    IdentityFile ~/.ssh/id_plain

Host equals
    HostName=equals.example.com
    User=bob

Host glob-* !glob-secret
    HostName globbed.example.com
    User carol

Host first
    User first-wins
Host first
    User second-loses
    Port 2200

Host keys
    IdentityFile ~/.ssh/one
    IdentityFile ~/.ssh/two

Host token
    HostName %h.example.com
    IdentityFile ~/.ssh/%h_key

Host *
    User fallback
    Port 22
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// The reader looks in two fixed places, so the test's file has to be one of them.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := filepath.Join(home, ".ssh", "config")
	if err := os.WriteFile(userConfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{
		"plain", "equals", "glob-one", "glob-secret", "first", "keys", "token", "unlisted",
	} {
		t.Run(host, func(t *testing.T) {
			want := paramikoLookup(t, userConfig, host)
			got := lookupSSHConfig(host)

			if s, _ := want["hostname"].(string); s != got.HostName {
				t.Errorf("hostname: paramiko says %q, borge says %q", s, got.HostName)
			}
			if s, _ := want["user"].(string); s != got.User {
				t.Errorf("user: paramiko says %q, borge says %q", s, got.User)
			}
			// paramiko reports no port as absent; borge reports it as empty, and the
			// caller supplies ssh's default in both tools.
			wantPort, _ := want["port"].(string)
			if wantPort != got.Port {
				t.Errorf("port: paramiko says %q, borge says %q", wantPort, got.Port)
			}
			var wantKeys []string
			if list, ok := want["identityfile"].([]any); ok {
				for _, item := range list {
					wantKeys = append(wantKeys, item.(string))
				}
			}
			if strings.Join(wantKeys, ",") != strings.Join(got.IdentityFiles, ",") {
				t.Errorf("identityfile: paramiko says %v, borge says %v", wantKeys, got.IdentityFiles)
			}
		})
	}
}

// TestSSHConfigCorpusIsNotVacuous: a corpus where every host fell through to "Host *" would
// agree with paramiko while testing none of the matching.
func TestSSHConfigCorpusIsNotVacuous(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host a\n HostName a.example.com\n IdentityFile ~/.ssh/ka\nHost b*\n User bee\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	exact := lookupSSHConfig("a")
	if exact.HostName != "a.example.com" {
		t.Errorf("an exact Host block did not apply: %+v", exact)
	}
	if len(exact.IdentityFiles) != 1 || strings.HasPrefix(exact.IdentityFiles[0], "~") {
		t.Errorf("IdentityFile was not expanded: %v", exact.IdentityFiles)
	}
	globbed := lookupSSHConfig("bravo")
	if globbed.User != "bee" {
		t.Errorf("a glob Host block did not apply: %+v", globbed)
	}
	unknown := lookupSSHConfig("nothing-matches-this")
	if unknown.HostName != "nothing-matches-this" || unknown.User != "" {
		t.Errorf("an unmatched host should be itself and nothing else: %+v", unknown)
	}
}

// TestSSHConfigMatchBlocksAreNotRead: paramiko supports Match and borge does not, so a
// Match block must not be read as part of whatever Host block came before it.
//
// Silently attaching its contents to the previous block would be worse than ignoring it:
// the connection would go somewhere the config never said. See DIVERGENCES #60.
func TestSSHConfigMatchBlocksAreNotRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host target\n HostName right.example.com\n" +
		"Match host target\n HostName wrong.example.com\n User wrong\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got := lookupSSHConfig("target")
	if got.HostName != "right.example.com" {
		t.Errorf("hostname is %q; a Match block leaked into the Host block above it", got.HostName)
	}
	if got.User != "" {
		t.Errorf("user is %q; a Match block leaked into the Host block above it", got.User)
	}
}
