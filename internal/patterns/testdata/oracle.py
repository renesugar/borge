# SPDX-License-Identifier: Apache-2.0
#
# Oracle for the pattern differential test.
#
# Compares *match results*, not regex strings: Go's RE2 and Python's re spell some
# constructs differently ("[^\/]" is valid in Python and an error in Go), so identical
# regexes are neither achievable nor the property that matters. What matters is that the
# same pattern selects the same paths.
#
# Line protocol, hex fields, "-" for empty:
#
#   T <pattern>              translate a shell pattern      -> OK <regex>
#   M <pattern> <string>     does it match?                 -> OK <1|0>
#   N <pattern> <name>       archive-name pattern match     -> OK <1|0>
#   F <pattern> <path>       fnmatch-style file pattern     -> OK <1|0>
#   P <style> <pattern> <path>   file pattern of a style    -> OK <1|0>
#   X <cmds> <path>          a whole matcher                -> OK <1|0>/<recurse 1|0>
#
# <cmds> is a comma-separated list of "<cmd><pattern>" entries as they would appear in a
# --patterns-from file, each hex-encoded.
#
# <pattern>, <string> and <name> are hex-encoded UTF-8, so they can carry spaces.

import re
import sys
import traceback

from borg.helpers import shellpattern
from borg.patterns import (
    FnmatchPattern,
    PathFullPattern,
    PathPrefixPattern,
    PatternMatcher,
    RegexPattern,
    ShellPattern,
    get_regex_from_pattern,
    parse_inclexcl_command,
)
from borg.patterns import IECommand

STYLES = {
    "fm": FnmatchPattern,
    "sh": ShellPattern,
    "re": RegexPattern,
    "pp": PathPrefixPattern,
    "pf": PathFullPattern,
}


def untext(s):
    return "" if s == "-" else bytes.fromhex(s).decode("utf-8")


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        p = line.split(" ")
        cmd = p[0]
        if cmd == "T":
            out = "OK " + shellpattern.translate(untext(p[1])).encode("utf-8").hex()
        elif cmd == "M":
            regex = re.compile(shellpattern.translate(untext(p[1])))
            out = "OK " + ("1" if regex.match(untext(p[2])) else "0")
        elif cmd == "N":
            regex = re.compile(get_regex_from_pattern(untext(p[1])) + r"\Z")
            out = "OK " + ("1" if regex.match(untext(p[2])) else "0")
        elif cmd == "F":
            out = "OK " + ("1" if FnmatchPattern(untext(p[1])).match(untext(p[2])) else "0")
        elif cmd == "P":
            pattern = STYLES[p[1]](untext(p[2]))
            out = "OK " + ("1" if pattern.match(untext(p[3])) else "0")
        elif cmd == "X":
            matcher = PatternMatcher(fallback=True)
            fallback = ShellPattern
            for spec in p[1].split(","):
                if not spec:
                    continue
                # Mirror parse_patternfile_line: a "P" line changes the default style for
                # the lines after it and is not itself a pattern.
                ie = parse_inclexcl_command(untext(spec), fallback=fallback)
                if ie.cmd is IECommand.PatternStyle:
                    fallback = ie.val
                elif ie.cmd is IECommand.RootPath:
                    pass
                else:
                    matcher.add_inclexcl([ie])
            matched = matcher.match(untext(p[2]))
            out = "OK {}/{}".format(1 if matched else 0, 1 if matcher.recurse_dir else 0)
        else:
            out = f"ERR unknown command {cmd!r}"
    except Exception as exc:
        detail = traceback.format_exc().strip().replace("\n", " | ")
        out = f"ERR {type(exc).__name__}: {exc} [{detail[-500:]}]"
    sys.stdout.write(out + "\n")
    sys.stdout.flush()
