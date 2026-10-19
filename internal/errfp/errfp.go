// Package errfp groups cli.error.raised events into failure causes: it
// normalizes an error's message and stack trace so that the same failure on
// two machines (different home directories, temp dirs, ids, line numbers)
// produces the same fingerprint.
//
// The algorithm, also described in docs/schema.md "Error fingerprints":
//
//  1. If the event has a stack trace and at least one frame can be read from
//     it, the fingerprint covers the innermost MaxFrames frames. Each frame
//     is reduced to its function and the base name of its file: directories
//     (home, temp and module cache paths), line and column numbers, and
//     program-counter offsets are dropped. Python tracebacks, which list the
//     innermost frame last, are reversed first.
//  2. Otherwise it covers the message, with variable parts replaced by
//     placeholders: quoted strings, UUIDs, URLs, absolute paths, hex ids and
//     numbers.
//  3. The fingerprint is the first 16 hex digits of SHA-256 over the
//     algorithm Version, the source ("stack" or "message") and the
//     normalized lines. A change to the algorithm bumps Version, so new
//     fingerprints never collide with ones stored under an older version.
//
// Fingerprints are computed once at ingest and stored in
// logs.error_fingerprint, so changing the algorithm only affects rows
// ingested afterwards.
package errfp

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The log event fingerprints are computed for, and the attributes they are
// computed from (both min_tier full in the allowlist schema).
const (
	EventName      = "cli.error.raised"
	AttrMessage    = "cli.error.message"
	AttrStacktrace = "cli.error.stacktrace"
)

// Version is mixed into every fingerprint; bump it whenever a change here
// would group events differently.
const Version = "v1"

// MaxFrames is how many of the innermost stack frames identify a failure.
const MaxFrames = 5

// Source says which part of the event a fingerprint was derived from.
const (
	SourceStack   = "stack"
	SourceMessage = "message"
)

// Result is the outcome of fingerprinting one event.
type Result struct {
	Fingerprint string
	Source      string
	Message     string // normalized message, "" if the event had none
	TopFrame    string // normalized innermost frame, "" if no stack was read
}

// Compute fingerprints an event from its raw message and stack trace. ok is
// false when neither yields anything to group on.
func Compute(message, stack string) (r Result, ok bool) {
	r.Message = NormalizeMessage(message)
	frames := Frames(stack)
	if len(frames) > 0 {
		r.TopFrame = frames[0]
		r.Source = SourceStack
		r.Fingerprint = hash(SourceStack, frames)
		return r, true
	}
	if r.Message == "" {
		return Result{}, false
	}
	r.Source = SourceMessage
	r.Fingerprint = hash(SourceMessage, []string{r.Message})
	return r, true
}

func hash(source string, lines []string) string {
	h := sha256.New()
	h.Write([]byte(Version + "\n" + source + "\n" + strings.Join(lines, "\n")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Truncate shortens s to at most n runes, marking a cut with "…".
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

// --- message normalization ---------------------------------------------

var (
	reQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`[^`]*`")
	reUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reURL    = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s'"<>()]+`)
	// An absolute path starts at the beginning of the text or after a
	// delimiter, so "read/write" isn't one: POSIX (/x, ~/x), Windows drive
	// (C:\x, C:/x) and UNC (\\host\share).
	rePath = regexp.MustCompile(`(^|[\s(\[{=,:;])(~?/[^\s'"()\[\]{},;:]+|[A-Za-z]:[\\/][^\s'"()\[\]{},;:]*|\\\\[^\s'"()\[\]{},;:]+)`)
	reHex  = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b|\b[0-9a-f]{8,}\b`)
	// Numbers, with dotted or colon-separated parts (versions, IPs, ports,
	// times) and a short duration or size unit.
	reNumber = regexp.MustCompile(`\b\d+(?:[.:]\d+)*(?:ns|us|ms|s|m|h|b|kb|mb|gb|k)?\b`)
	reSpace  = regexp.MustCompile(`\s+`)
)

// NormalizeMessage replaces the variable parts of an error message with
// placeholders and collapses whitespace.
func NormalizeMessage(s string) string {
	s = reQuoted.ReplaceAllString(s, "<str>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reURL.ReplaceAllString(s, "<url>")
	s = rePath.ReplaceAllString(s, "$1<path>")
	s = reHex.ReplaceAllStringFunc(s, func(m string) string {
		// A bare run of hex digits is only an id if it mixes digits and
		// letters; all digits is a number, all letters a word.
		if strings.HasPrefix(strings.ToLower(m), "0x") || (strings.ContainsAny(m, "0123456789") && strings.ContainsAny(strings.ToLower(m), "abcdef")) {
			return "<hex>"
		}
		return m
	})
	s = reNumber.ReplaceAllString(s, "<n>")
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}

// --- stack frames ------------------------------------------------------

var (
	rePythonFrame = regexp.MustCompile(`^File "([^"]*)", line \d+(?:, in (.+))?$`)
	// A source location: a file name with an extension, then :line[:col],
	// behind any directories or URL scheme. Group 1 is the file name.
	reLocation = regexp.MustCompile(`[^\s()'"]*?([^\s()'"/\\]+\.[A-Za-z0-9]+):\d+(?::\d+)?`)
	reRubyIn   = regexp.MustCompile("^:in [`']([^`']+)'")
	// .NET: "at Ns.T.M(args) in C:\\src\\T.cs:line 40".
	reDotnetIn = regexp.MustCompile(`^(.*) in (.+):line \d+$`)
	// A location without a file extension, such as Node's "(node:fs:601:3)".
	reBareLocation = regexp.MustCompile(`\(([^\s()]+?):\d+:\d+\)$`)
	reGoSuffix     = regexp.MustCompile(`\s+in goroutine \d+$`)
	reOffset       = regexp.MustCompile(`\s*\+0x[0-9a-fA-F]+$`)
)

// Frames extracts the innermost MaxFrames frames of a stack trace,
// innermost first, each normalized to "function file" (either part may be
// missing). It returns nil when no line looks like a frame.
func Frames(stack string) []string {
	lines := strings.Split(strings.ReplaceAll(stack, "\r\n", "\n"), "\n")
	var frames []string
	python := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Traceback (most recent call last)") {
			python = true
			continue
		}
		if m := rePythonFrame.FindStringSubmatch(line); m != nil {
			frames = append(frames, frame(m[2], baseName(m[1])))
			continue
		}
		// Go prints a function line followed by an indented line that is
		// only a location; the pair is one frame.
		if i+1 < len(lines) && isIndented(lines[i+1]) && !reLocation.MatchString(line) {
			next := strings.TrimSpace(lines[i+1])
			if loc := reLocation.FindStringSubmatchIndex(next); loc != nil && loc[0] == 0 {
				frames = append(frames, frame(goFunc(line), next[loc[2]:loc[3]]))
				i++
				continue
			}
		}
		if f, ok := genericFrame(line); ok {
			frames = append(frames, f)
		}
	}
	if python {
		slices.Reverse(frames)
	}
	if len(frames) > MaxFrames {
		frames = frames[:MaxFrames]
	}
	return frames
}

// goFunc reduces a Go traceback function line, "pkg.(*T).F(0xc000, 0x1)" or
// "created by pkg.F in goroutine 7", to the function name.
func goFunc(line string) string {
	line = strings.TrimPrefix(line, "created by ")
	line = reGoSuffix.ReplaceAllString(line, "")
	// The arguments are the last parenthesized group; earlier ones belong
	// to the name, as in "(*T)".
	if i := strings.LastIndexByte(line, '('); i > 0 && strings.HasSuffix(line, ")") {
		line = line[:i]
	}
	return line
}

// genericFrame reads the "at fn (file:line:col)" family (JavaScript, Java,
// .NET, Rust) and any other line carrying a file:line location (Ruby, bare
// locations).
func genericFrame(line string) (string, bool) {
	rest, at := strings.CutPrefix(line, "at ")
	loc := reLocation.FindStringSubmatchIndex(rest)
	if !at && loc == nil {
		return "", false
	}
	fn, file := rest, ""
	if m := reDotnetIn.FindStringSubmatch(rest); m != nil {
		return frame(m[1], baseName(m[2])), true
	}
	if m := reBareLocation.FindStringSubmatchIndex(rest); loc == nil && m != nil {
		return frame(rest[:m[0]], rest[m[2]:m[3]]), true
	}
	if loc != nil {
		file = rest[loc[2]:loc[3]]
		fn = rest[:loc[0]]
		if m := reRubyIn.FindStringSubmatch(rest[loc[1]:]); m != nil && strings.TrimSpace(fn) == "" {
			fn = m[1]
		}
	}
	// Strip the "(" or "@" that separates a function from its location.
	fn = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(fn), "(@"))
	return frame(fn, file), true
}

// frame joins a frame's function and file after normalizing both: a file
// name can carry a build hash ("chunk-3f2a9b1c.js"), a function an address.
func frame(fn, file string) string {
	fn = NormalizeMessage(reOffset.ReplaceAllString(fn, ""))
	return strings.TrimSpace(fn + " " + NormalizeMessage(file))
}

func isIndented(line string) bool {
	return strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "  ")
}

// baseName is the last path element of a POSIX or Windows path.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
