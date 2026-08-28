// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The environment topic makes claims about how variables are read and when borge asks for
// a passphrase. Until now those sentences were true and nothing checked them, which is the
// state four documentation claims were in when two of them went false during stage 8.
// These are the checks the doc anchors register against.

// TestEnvVariablesAreReadUnderBothPrefixes pins the sentence the environment topic opens
// with: BORGE_<NAME> first, BORG_<NAME> second.
//
// The rule lives in one place - Env.lookupBorg - so this tests the mechanism rather than
// enumerating variables. The reverse direction, that every variable the code reads is
// documented, is TestHelpEnvironmentTopicListsEveryVariable.
//
//borge:checks environment/prefix-fallback
func TestEnvVariablesAreReadUnderBothPrefixes(t *testing.T) {
	env := func(vars map[string]string) *Env {
		return &Env{Getenv: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		}}
	}

	// The BORG_ spelling alone is honoured, which is what makes an existing borg setup
	// work unchanged.
	e := env(map[string]string{"BORG_REPO": "/borg/repo"})
	if got, ok := e.lookupBorg("REPO"); !ok || got != "/borg/repo" {
		t.Errorf("with only BORG_REPO set, lookupBorg(REPO) = %q, %v; want the borg spelling", got, ok)
	}

	// BORGE_ wins when both are set, which is what lets one machine run both tools.
	e = env(map[string]string{"BORGE_REPO": "/borge/repo", "BORG_REPO": "/borg/repo"})
	if got, _ := e.lookupBorg("REPO"); got != "/borge/repo" {
		t.Errorf("with both set, lookupBorg(REPO) = %q, want the BORGE_ spelling to win", got)
	}

	// An empty BORGE_ value is a value, not an absence: a user who exports BORGE_REPO=""
	// has said something, and falling through to BORG_REPO would act on a repository they
	// did not name.
	e = env(map[string]string{"BORGE_REPO": "", "BORG_REPO": "/borg/repo"})
	if got, ok := e.lookupBorg("REPO"); !ok || got != "" {
		t.Errorf("an empty BORGE_REPO gave %q, %v; want the empty value to be honoured", got, ok)
	}

	// Neither set is an absence, and the caller must be able to tell it apart from empty.
	if _, ok := env(nil).lookupBorg("REPO"); ok {
		t.Error("lookupBorg reported a value with nothing set")
	}
}

// TestPassphrasePromptingFollowsTheTopic checks the paragraph about prompting: the
// unencrypted modes never prompt, and with no terminal borge says which variable to set
// rather than hanging.
//
// The sentence this replaces was false for a while - "borge never prompts for a
// passphrase", left in the topic after prompting was implemented - and a human had to
// notice. The environment in a test has no terminal, so it is exactly the cron-job path
// the topic describes.
//
//borge:checks environment/passphrase-prompt
func TestPassphrasePromptingFollowsTheTopic(t *testing.T) {
	// An unencrypted repository has nothing to unlock, so no passphrase is asked for and
	// none is needed. No archives are made: repo-list opens the repository, which is where
	// a passphrase would be wanted, and archives would only make the test slower.
	plain := newBorgRepo(t, "none-sha256")
	var stdout, stderr bytes.Buffer
	e := plain.borgeEnv(&stdout, &stderr)
	// Not even a passphrase in the environment: the topic says the unencrypted modes
	// never prompt, and a variable set would hide a prompt that did happen.
	e.Getenv = func(name string) (string, bool) {
		switch name {
		case "BORGE_REPO":
			return plain.path, true
		case "BORGE_KEYS_DIR":
			return plain.keysDir, true
		case "BORGE_TESTONLY_WEAKEN_KDF":
			return "1", true
		}
		return "", false
	}
	if code := Run(e, []string{"repo-list"}); code != ExitOK {
		t.Fatalf("repo-list on an unencrypted repository exited %d\nstderr: %s", code, stderr.String())
	}
	if strings.Contains(strings.ToLower(stderr.String()), "passphrase") {
		t.Errorf("an unencrypted repository mentioned a passphrase: %q", stderr.String())
	}

	// An encrypted repository with no passphrase and no terminal: the error has to name
	// the variable to set. Exiting non-zero is not enough - "wrong passphrase" with no
	// way forward is the failure this sentence exists to prevent.
	locked := newBorgRepo(t, "aes256-ocb")
	stdout.Reset()
	stderr.Reset()
	e = locked.borgeEnv(&stdout, &stderr)
	e.Getenv = func(name string) (string, bool) {
		switch name {
		case "BORGE_REPO":
			return locked.path, true
		case "BORGE_KEYS_DIR":
			return locked.keysDir, true
		case "BORGE_TESTONLY_WEAKEN_KDF":
			return "1", true
		}
		return "", false
	}
	code := Run(e, []string{"repo-list"})
	if code == ExitOK {
		t.Fatalf("repo-list on an encrypted repository succeeded with no passphrase:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "BORGE_PASSPHRASE") {
		t.Errorf("the error does not name the variable to set: %q", stderr.String())
	}
}
