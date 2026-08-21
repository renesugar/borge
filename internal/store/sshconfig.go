// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of the ssh config lookup in borgstore/backends/sftp.py
// (borgstore 0.6.1, BSD 3-Clause, Copyright (C) 2026 Thomas Waldmann) - which files are
// read, in which order, and which of their values decide a connection.
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.
//
// The ssh_config rules themselves are OpenSSH's documented ones. paramiko - which borg
// reaches this file through - is used here as an *oracle*: the tests compare borge's answers
// against it case by case. No paramiko code is copied or translated, and this file is not a
// derivative of it; see docs/LICENSING.md §7.

package store

import (
	"bufio"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Reading ~/.ssh/config, because an sftp:// URL is usually an alias and nothing else.
//
// "sftp://backup-server/repo" names a Host block that supplies the real hostname, the user,
// the port and the key. borg reaches that through paramiko, so paramiko's rules are the
// contract here, not OpenSSH's: where the two differ, a URL has to keep working when a user
// moves between the tools. TestSSHConfigMatchesParamiko compares them case by case.
//
// # Behaviour, and its limits
//
// Supported: Host blocks with glob patterns and negation, "key value" and "key=value",
// comments, quoted values, and the four keys that decide a connection - HostName, User,
// Port, IdentityFile. First value wins, as in OpenSSH; IdentityFile accumulates across
// matching blocks. The tokens paramiko expands for these keys are expanded here.
//
// Not supported, because paramiko does not support them either: Include. Match blocks are
// paramiko's and are *not* implemented here - a config that uses one gets less than borg
// would give it, which is recorded in DIVERGENCES.md #60 rather than papered over.
//
// # Why paramiko decides what "correct" means here
//
// borg reaches an sftp:// repository through borgstore, which reaches it through paramiko.
// So where paramiko and OpenSSH disagree, paramiko's answer is the one that decides where
// borg connects, and matching OpenSSH instead would send borge somewhere else.
// TestSSHConfigMatchesParamiko compares the two over the shapes a real config has.

// sshConfigKeys are the only directives this reads. Everything else in a config file is
// parsed and ignored, which is what lets an ordinary config with fifty directives in it be
// used for the four values a connection needs.
var sshConfigKeys = map[string]bool{
	"hostname":     true,
	"user":         true,
	"port":         true,
	"identityfile": true,
}

// sshHostConfig is what a lookup produces.
type sshHostConfig struct {
	HostName      string
	User          string
	Port          string
	IdentityFiles []string
}

// lookupSSHConfig assembles the configuration for a host, from the system file and then the
// user's - the user's winning, which is borgstore's order.
func lookupSSHConfig(host string) sshHostConfig {
	merged := map[string][]string{}
	for _, path := range []string{"/etc/ssh/ssh_config", filepath.Join(homeDir(), ".ssh", "config")} {
		for key, values := range readSSHConfig(path, host) {
			// A later file replaces a key outright rather than adding to it, because
			// borgstore merges the two dictionaries in this order.
			merged[key] = values
		}
	}
	out := sshHostConfig{
		HostName:      firstValue(merged, "hostname"),
		User:          firstValue(merged, "user"),
		Port:          firstValue(merged, "port"),
		IdentityFiles: merged["identityfile"],
	}
	if out.HostName == "" {
		// paramiko's rule: with no HostName configured, the name being looked up is the
		// hostname. Applied before token expansion, as it is there.
		out.HostName = host
	}
	out.HostName = expandSSHTokens(out.HostName, "hostname", host, out)
	for i, file := range out.IdentityFiles {
		out.IdentityFiles[i] = expandSSHTokens(file, "identityfile", host, out)
	}
	return out
}

// readSSHConfig reads one file and returns the values that apply to host.
func readSSHConfig(path, host string) map[string][]string {
	file, err := os.Open(path)
	if err != nil {
		// A missing file is the ordinary case, not a failure: most machines have no
		// /etc/ssh/ssh_config entry for this host and many have no user config at all.
		return nil
	}
	defer file.Close()

	out := map[string][]string{}
	applies := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := splitSSHConfigLine(scanner.Text())
		if !ok {
			continue
		}
		switch key {
		case "host":
			applies = sshPatternsMatch(splitFields(value), host)
			continue
		case "match":
			// Not implemented (#60). A Match block must at least stop the previous Host
			// block from swallowing its contents, or a config using one would be read
			// wrongly rather than incompletely.
			applies = false
			continue
		}
		if !applies || !sshConfigKeys[key] {
			continue
		}
		if key == "identityfile" {
			// Several keys may be offered, and repeats accumulate rather than replace.
			for _, file := range splitFields(value) {
				if !contains(out[key], file) {
					out[key] = append(out[key], file)
				}
			}
			continue
		}
		if len(out[key]) == 0 {
			// First value wins, which is OpenSSH's rule and paramiko's.
			out[key] = []string{value}
		}
	}
	return out
}

// splitSSHConfigLine splits one line into a lowercased key and its value, or reports that
// there is nothing on it.
func splitSSHConfigLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	// "Key value", "Key=value" and "Key = value" are all the same thing.
	key, value, found := strings.Cut(line, "=")
	if found && !strings.ContainsAny(strings.TrimSpace(key), " \t") {
		return strings.ToLower(strings.TrimSpace(key)), unquoteSSHValue(strings.TrimSpace(value)), true
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0]))
	return strings.ToLower(fields[0]), unquoteSSHValue(rest), true
}

func unquoteSSHValue(value string) string {
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		return value[1 : len(value)-1]
	}
	return value
}

// splitFields splits a value into words, honouring double quotes as ssh does.
func splitFields(value string) []string {
	var out []string
	var current strings.Builder
	inQuotes := false
	for _, r := range value {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case (r == ' ' || r == '\t' || r == ',') && !inQuotes:
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// sshPatternsMatch reports whether a Host line's patterns cover this host.
//
// A negated pattern that matches wins outright: "Host * !secret" applies to everything but
// secret, and getting that backwards would send a connection to the wrong machine.
func sshPatternsMatch(patterns []string, host string) bool {
	matched := false
	for _, pattern := range patterns {
		if negated, ok := strings.CutPrefix(pattern, "!"); ok {
			if globMatch(negated, host) {
				return false
			}
			continue
		}
		if globMatch(pattern, host) {
			matched = true
		}
	}
	return matched
}

// globMatch is ssh's pattern matching: "*" for any run of characters, "?" for one.
//
// filepath.Match is close but not the same - it refuses to let "*" cross a "/", and a host
// alias may contain one.
func globMatch(pattern, s string) bool {
	// A straightforward backtracking match, which is enough for patterns of this size.
	var star, matched int
	i, j := 0, 0
	star = -1
	for i < len(s) {
		switch {
		case j < len(pattern) && (pattern[j] == '?' || pattern[j] == s[i]):
			i++
			j++
		case j < len(pattern) && pattern[j] == '*':
			star, matched = j, i
			j++
		case star >= 0:
			j = star + 1
			matched++
			i = matched
		default:
			return false
		}
	}
	for j < len(pattern) && pattern[j] == '*' {
		j++
	}
	return j == len(pattern)
}

// expandSSHTokens replaces the % tokens paramiko replaces, for the key being expanded.
//
// The table is per key, which looks arbitrary and is OpenSSH's: HostName takes only %h,
// IdentityFile takes ~, %d, %h, %u and %r. Expanding more than that would resolve a token
// borg leaves alone.
func expandSSHTokens(value, key, targetHost string, config sshHostConfig) string {
	if value == "" {
		return value
	}
	hostname := config.HostName
	if key == "hostname" {
		// Expanding HostName with itself would substitute a value that may carry a token.
		hostname = targetHost
	}
	user := config.User
	if user == "" {
		user = localUsername()
	}
	replace := func(token, with string) {
		value = strings.ReplaceAll(value, token, with)
	}
	switch key {
	case "hostname":
		replace("%h", hostname)
	case "identityfile":
		if strings.HasPrefix(value, "~") {
			value = filepath.Join(homeDir(), strings.TrimPrefix(value, "~"))
		}
		replace("%d", homeDir())
		replace("%h", hostname)
		replace("%u", localUsername())
		replace("%r", user)
	}
	return value
}

func firstValue(values map[string][]string, key string) string {
	if list := values[key]; len(list) > 0 {
		return list[0]
	}
	return ""
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

func localUsername() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
