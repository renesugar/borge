// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file implements borg's --log-json: the JsonFormatter and the log-record fields of
// src/borg/logger.py, against the wire format specified in borg's
// docs/internals/frontends.rst.
// Original work Copyright (C) 2015-2026 The Borg Collective; Copyright (C) 2010-2014 Jonas Borgström.
// Licensed under the BSD 3-Clause License, see licenses/borg/LICENSE.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// --log-json turns stderr into a stream of JSON objects, one per line, each tagged with a
// "type". It is the other half of borg's frontend API: --json gives a frontend the
// command's result, --log-json gives it everything the command said on the way.
//
// The rule that shapes the implementation is that it has to be *all* of stderr. A frontend
// reads the stream line by line and parses each one; a single plain-text line in the
// middle is a parse error, which is worse than not offering the option at all. So rather
// than converting the hundred-odd call sites one at a time and hoping none was missed,
// stderr itself is wrapped: anything written to it that did not come from a level-aware
// helper becomes an INFO log_message. A message borge has not thought about yet still
// comes out as valid JSON.
//
// # What borge does not emit, and why
//
// borg has three progress types - archive_progress, progress_message and progress_percent
// - and the frontends document says they are "not produced unless --progress is specified".
// borge has no --progress at all (it is one of the absent common options), so there is no
// progress to report and no place to report it from. When --progress lands, these types
// come with it.
//
// The prompt types (question_prompt and its relatives) are likewise absent: borge's only
// prompt is for a passphrase, and it is written to the terminal rather than to stderr.
//
// Both absences are silence rather than wrong output: a frontend that sees no progress
// objects learns nothing false.

// logRecord is borg's log_message object. The field order here is the order borg emits,
// which JSON does not care about but a person diffing two streams does.
type logRecord struct {
	Type      string  `json:"type"`
	Time      float64 `json:"time"`
	Message   string  `json:"message"`
	LevelName string  `json:"levelname"`
	Name      string  `json:"name"`
}

// unixTime is borg's timestamp: seconds since the epoch as a float, not an ISO string.
// The JSON API uses both forms, in different places, and this is the one the log stream
// uses.
func unixTime(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// jsonLogger renders log records, and is nil when --log-json was not given.
type jsonLogger struct {
	out io.Writer
	// name is what borg calls the emitting entity. borg uses its Python logger names
	// ("borg.archiver.create_cmd", "borg.repository"); borge has no logger hierarchy, so
	// it reports the command, which is the part a frontend can act on.
	name string
	// buf holds a partial line: a Write does not have to end on a newline.
	buf bytes.Buffer
}

// Write turns plain output into log_message objects, one per line.
//
// Splitting on newlines is a compromise worth naming. borg emits one object per *log
// call*, so a multi-line warning is one object with newlines inside its message; borge
// can only see bytes here, so it emits one object per line. Every line is still a valid
// object of the right type, which is the property a frontend depends on - the granularity
// differs, the contract does not. Messages that go through logf keep their whole shape.
func (l *jsonLogger) Write(p []byte) (int, error) {
	n := len(p)
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadString('\n')
		if err != nil {
			// No newline yet: keep the remainder for the next Write.
			l.buf.Reset()
			l.buf.WriteString(line)
			return n, nil
		}
		l.emit("INFO", strings.TrimRight(line, "\n"))
	}
}

// emit writes one log_message. An empty message is dropped rather than turned into an
// object saying nothing: borge prints blank lines to space its output, and a frontend
// reading the stream has no use for them.
func (l *jsonLogger) emit(level, message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	l.write(logRecord{
		Type:      "log_message",
		Time:      unixTime(time.Now()),
		Message:   message,
		LevelName: level,
		Name:      l.name,
	})
}

func (l *jsonLogger) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		// Nothing useful to do: this is the error path of the error path. Fall back to
		// text rather than losing the message entirely.
		fmt.Fprintf(l.out, "%v\n", v)
		return
	}
	l.out.Write(append(data, '\n'))
}

// flush emits whatever is left in the buffer, for output that did not end in a newline.
func (l *jsonLogger) flush() {
	if l.buf.Len() > 0 {
		l.emit("INFO", l.buf.String())
		l.buf.Reset()
	}
}

// enableJSONLog switches this Env's stderr to the JSON form. Called from flagSet.Parse
// once parsing has succeeded, which reproduces borg's documented caveat exactly: "JSON
// logging requires successful argument parsing. Even with --log-json specified, a parsing
// error will be printed in plain text, because logging set-up happens after all arguments
// are parsed."
func (e *Env) enableJSONLog(command string) {
	if e.logger != nil {
		return
	}
	name := "borge"
	if command != "" {
		name = "borge." + strings.ReplaceAll(command, " ", ".")
	}
	e.logger = &jsonLogger{out: e.Stderr, name: name}
	e.Stderr = e.logger
}

// setStatusFilter records --filter STATUSCHARS: only these statuses are listed.
//
// borg keeps it on the archiver object as output_filter and checks it in one place, which
// is why the option costs nothing on the commands that have it. borge does the same rather
// than threading a predicate through each command's options.
func (e *Env) setStatusFilter(chars string) { e.statusFilter = chars }

// logFileStatus reports one item the way --list does, in whichever form is in force.
func (e *Env) logFileStatus(status byte, path string) {
	// borg's condition is "output_list and status is not None and (output_filter is None
	// or status in output_filter)". The caller has already decided about --list.
	if e.statusFilter != "" && !strings.ContainsRune(e.statusFilter, rune(status)) {
		return
	}
	if e.logger != nil {
		// The path goes through borg's text_to_json here as everywhere else: a name that
		// is not valid unicode gets an approximation plus path_b64. borg does this for
		// file_status too (archiver/__init__.py), and it is the one place the frontend
		// learns which file is being worked on.
		rec := map[string]any{"type": "file_status", "status": string(status)}
		putText(rec, "path", path)
		e.logger.write(rec)
		return
	}
	fmt.Fprintf(e.Stderr, "%c %s\n", status, path)
}

// groupHelpRequested reports whether a command group's first argument asks for the group's
// own help rather than naming a subcommand.
//
// The same gap as takeParentLogJSON below and found the same way: "debug", "key" and
// "benchmark" build no FlagSet, so nothing in them had ever seen an option. borg's groups
// are argparse parsers, so "borg debug --help" prints the group's usage and exits 0; borge
// answered 'unknown debug command "--help"' on stderr and exited 2 until 2026-08-20.
//
// It surfaced while making command-coverage.sh descend into the groups: the gate has to ask
// borge for its subcommand list the way it asks borg, and the obvious way to ask was the
// one spelling that did not work.
func groupHelpRequested(arg string) bool {
	// The three spellings Go's flag package answers, so that a group and a command agree
	// on what asking for help looks like.
	return arg == "-h" || arg == "-help" || arg == "--help"
}

// takeParentLogJSON handles --log-json given to a command *group* - "debug", "key",
// "benchmark" - before the subcommand name.
//
// Those three dispatch straight to a subcommand and build no FlagSet of their own, so they
// never reach newFlagSet and never saw the option. borg accepts it there and honours it:
//
//	$ borg debug --log-json dump-manifest -r /tmp/nope
//	{"type": "log_message", ..., "message": "Repository ... does not exist.",
//	 "levelname": "ERROR", ...}
//
// while borge answered "unknown debug command \"--log-json\"" until 2026-08-19. A frontend
// putting the option where borg's own help shows it got an error instead of a stream.
//
// Only this one option is taken here, not a parent-level parse of everything: borg's group
// parsers accept the whole common set, and borge implements one of those fourteen. A
// general parse would also have to know which of the remaining arguments belong to the
// subcommand, which is the subcommand's business.
func (e *Env) takeParentLogJSON(group string, args []string) []string {
	out := args[:0:0]
	for _, arg := range args {
		if arg == "--log-json" || arg == "-log-json" {
			e.enableJSONLog(group)
			continue
		}
		out = append(out, arg)
	}
	return out
}

// groupUsageOptions is the options section a command group prints. The group itself takes
// no options of its own, only the one every command takes, and it has to say so: borge
// accepts "--log-json" ahead of the subcommand (see takeParentLogJSON) and an option that
// works but appears in no help is one nobody finds.
const groupUsageOptions = "\noptions:\n" +
	"  --log-json           write stderr as one JSON object per line instead of text\n"
