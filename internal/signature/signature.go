// Package signature reduces a test failure's output to a stable identifier, so
// that failures with the same cause can be grouped and failures with different
// causes cannot.
//
// # PREFER SPLITTING OVER MERGING
//
// This is the rule every judgement call in this file is decided by.
//
// A cluster that splits one bug into two is VISIBLE. The user sees two clusters
// whose text is nearly identical, notices, and reads both. The cost is a little
// duplicated reading.
//
// A cluster that merges two distinct bugs is SILENT. Nothing in the report says
// a second failure was folded into the first. The user fixes one cause, reruns,
// and the other is still there - and the report that hid it is the reason they
// did not look. The cost is a bug shipped.
//
// The two are not symmetric, so the normalizations here are deliberately few
// and deliberately narrow. In particular integers in MESSAGES are not
// normalized. "got 1234, want 1000" and "got 7, want 1000" are probably the
// same bug, and a rule that merged them would read well on that example - but
// the same rule merges "got 3, want 4" with "got 9, want 4" when those are two
// different bugs, and nothing would ever tell you it had.
//
// # What is normalized
//
// Exactly these classes, and no more:
//
//	goroutine IDs      goroutine 42 [running]:  ->  goroutine N [running]:
//	hex addresses      0x00c000192              ->  0xADDR
//	frame offsets      +0x1c                    ->  +0xOFF
//	build temp paths   /tmp/go-build123456/     ->  /tmp/go-build/
//	durations          1.003s, 12ms             ->  Ns
//	module cache       /root/go/pkg/mod/        ->  MODCACHE/
//
// # What this cannot do
//
// Two limits, both on the splitting side, both stated rather than papered over:
//
// The stream does not distinguish t.Logf output from t.Errorf output - both
// arrive as "file.go:NN: text". A failing test that also logs will carry its
// log lines into its signature, so a log line that varies between runs splits
// the failure.
//
// The race detector chooses which of a racing pair to call the "Read"/"Write"
// and which the "Previous write", and when several races are live it reports
// them in whatever order it caught them. The same underlying race can therefore
// produce two different first reports and land in two clusters.
package signature

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Kind is the shape of failure the output was recognised as. It is part of the
// hashed text, so an assertion whose message happens to read like a panic
// message cannot collide with the panic.
type Kind int

const (
	// KindUnknown: the output matched none of the shapes below. Its signature
	// is the whole normalized output, which is stable but discriminating - the
	// splitting side of the tradeoff, chosen on purpose for the case where
	// flakescope does not understand what it is looking at.
	KindUnknown Kind = iota
	// KindAssertion: no stack. `file_test.go:42: message`, from t.Errorf or
	// t.Fatalf. This is the majority of real failures, which is why clustering
	// that handled only panics would not be clustering worth having.
	KindAssertion
	// KindPanic: a message followed by one or more goroutine stacks.
	KindPanic
	// KindRace: the race detector's own layout - "WARNING: DATA RACE" and a
	// pair of stacks for the two conflicting accesses.
	KindRace
)

func (k Kind) String() string {
	switch k {
	case KindAssertion:
		return "assertion"
	case KindPanic:
		return "panic"
	case KindRace:
		return "race"
	default:
		return "unknown"
	}
}

// Frames is the default number of stack frames a panic or race signature keeps.
//
// Five reaches past the testing and runtime frames at the top of a Go panic
// stack and into the code that actually failed, without reaching so far down
// that two failures at the same site are separated by their callers.
const Frames = 5

// Signature identifies one failure.
type Signature struct {
	// Hash is the identifier: sha256 of Normalized, truncated to 8 bytes and
	// rendered as 16 hex characters.
	//
	// sha256 rather than FNV because this string is printed to users and
	// written into the JSON report. A collision there produces a report that
	// merges two bugs - the silent failure this package exists to avoid - and
	// FNV's collision behaviour over adversarial-looking input is not something
	// to have to reason about in a bug report.
	Hash string
	// Kind is the shape the output was recognised as.
	Kind Kind
	// Normalized is the exact text that was hashed, kept so that two
	// signatures that should have matched can be diffed.
	Normalized string
}

// Of returns the signature of a failure's output lines, keeping Frames stack
// frames.
func Of(output []string) Signature { return OfFrames(output, Frames) }

// OfFrames is Of with the frame count chosen by the caller. frames is clamped
// at zero: a negative count keeps no frames rather than all of them.
func OfFrames(output []string, frames int) Signature {
	if frames < 0 {
		frames = 0
	}
	lines := splitLines(output)
	kind := classify(lines)

	var body []string
	switch kind {
	case KindRace:
		body = raceBody(lines, frames)
	case KindPanic:
		body = panicBody(lines, frames)
	case KindAssertion:
		body = assertionBody(lines)
	default:
		// Nothing was recognised, so nothing may be discarded.
		for _, line := range lines {
			body = append(body, normalize(strings.TrimRight(line, " \t")))
		}
	}

	normalized := kind.String() + "\n" + strings.Join(body, "\n")
	sum := sha256.Sum256([]byte(normalized))
	return Signature{
		Hash:       hex.EncodeToString(sum[:8]),
		Kind:       kind,
		Normalized: normalized,
	}
}

// splitLines flattens the output events into lines. Each event usually carries
// exactly one line including its newline, but nothing in the stream guarantees
// that, so the events are joined and re-split rather than trusted.
func splitLines(output []string) []string {
	joined := strings.Join(output, "")
	if joined == "" {
		return nil
	}
	lines := strings.Split(joined, "\n")
	// Split leaves a trailing empty element for text ending in a newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

var (
	// A test log or assertion line: "    file_test.go:42: message". The colon
	// and space after the line number are what separate this from a stack
	// frame's file line, which is followed by an offset or by nothing.
	assertionRE = regexp.MustCompile(`^[ \t]*([^ \t:]+\.go):(\d+): (.*)$`)
	// A stack frame's file line, in either the panic layout (leading tab) or
	// the race detector's (leading spaces).
	frameFileRE = regexp.MustCompile(`^[ \t]+(\S+\.(?:go|s)):(\d+)(?: (\+0x[0-9a-fA-F]+))?$`)
	// The header a panic's goroutine stack starts with.
	goroutineRE = regexp.MustCompile(`^goroutine \d+ \[.*\]:$`)
	// The header of one of the two conflicting accesses in a race report.
	// "Previous" and the atomic variants are all of the forms the detector
	// emits; the goroutine-creation stacks that follow are deliberately not
	// matched here - see raceBody.
	raceAccessRE = regexp.MustCompile(`^(?:Previous )?(?:Atomic )?(?:[Rr]ead|[Ww]rite) at .* by .*:$`)
	raceRuleRE   = regexp.MustCompile(`^=+$`)
)

func classify(lines []string) Kind {
	// Race before panic: the detector's report ends by failing the test through
	// testing, which prints a "testing.go:NNNN: race detected" line that would
	// otherwise read as an assertion.
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "WARNING: DATA RACE") {
			return KindRace
		}
	}
	for _, line := range lines {
		if isPanicMessage(line) {
			return KindPanic
		}
	}
	for _, line := range lines {
		if assertionRE.MatchString(line) {
			return KindAssertion
		}
	}
	return KindUnknown
}

func isPanicMessage(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "panic: ") || strings.HasPrefix(t, "fatal error: ")
}

// panicBody is the panic message, the goroutine header if there is one, and the
// top `frames` frames.
//
// Only the first message line is kept. Go prints a recovered panic twice -
// "panic: X [recovered]" and then "panic: X" - and the second says nothing the
// first did not.
func panicBody(lines []string, frames int) []string {
	var body []string
	start := len(lines)
	for i, line := range lines {
		if isPanicMessage(line) {
			body = append(body, normalize(strings.TrimSpace(line)))
			start = i + 1
			break
		}
	}
	rest := lines[start:]
	for i, line := range rest {
		if goroutineRE.MatchString(strings.TrimSpace(line)) {
			body = append(body, normalize(strings.TrimSpace(line)))
			rest = rest[i+1:]
			break
		}
	}
	return append(body, topFrames(rest, frames)...)
}

// raceBody is "WARNING: DATA RACE" and, for each of the conflicting accesses in
// the FIRST report, that access's header and its top `frames` frames.
//
// Both access stacks are included, not just the first: a race is identified by
// the pair of sites, and a report that kept only one would merge every race
// that happens to be read from the same place.
//
// The goroutine-creation stacks that follow, and any second report, are not
// included. Which races the detector catches and in what order depends on
// timing, so folding them in would split one bug across configurations for a
// reason that has nothing to do with the bug.
func raceBody(lines []string, frames int) []string {
	start := 0
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "WARNING: DATA RACE") {
			start = i
			break
		}
	}
	report := lines[start:]
	for i, line := range report[1:] {
		if raceRuleRE.MatchString(strings.TrimSpace(line)) {
			report = report[:i+1]
			break
		}
	}

	body := []string{"WARNING: DATA RACE"}
	for i, line := range report {
		if !raceAccessRE.MatchString(strings.TrimSpace(line)) {
			continue
		}
		body = append(body, normalize(strings.TrimSpace(line)))
		body = append(body, topFrames(stackAfter(report, i), frames)...)
	}
	return body
}

// stackAfter returns the lines of the stack that begins at header index i, which
// runs until the next header or blank line.
func stackAfter(report []string, i int) []string {
	rest := report[i+1:]
	for j, line := range rest {
		t := strings.TrimSpace(line)
		if t == "" || raceRuleRE.MatchString(t) || strings.HasSuffix(t, ":") && !frameFileRE.MatchString(line) {
			return rest[:j]
		}
	}
	return rest
}

// topFrames returns up to n frames, normalized.
//
// A frame is a function line plus the file line under it. The file line is what
// is recognised - the function line is whatever non-blank line precedes it -
// because the two stack layouts Go emits indent the function differently but
// agree on the shape of the file line.
func topFrames(lines []string, n int) []string {
	var out []string
	found := 0
	for i, line := range lines {
		if found == n {
			break
		}
		if !frameFileRE.MatchString(line) {
			continue
		}
		fn := ""
		for j := i - 1; j >= 0; j-- {
			if strings.TrimSpace(lines[j]) != "" {
				fn = strings.TrimSpace(lines[j])
				break
			}
		}
		out = append(out, normalize(fn), normalize(strings.TrimSpace(line)))
		found++
	}
	return out
}

// assertionBody is every assertion-shaped line, normalized and stripped of the
// indentation the testing package adds per subtest level.
//
// Every such line is kept, not just the first. A test that fails at line 10 and
// a test that fails at lines 10 and 20 are not the same failure, and keeping
// only the first would merge them.
func assertionBody(lines []string) []string {
	var body []string
	for _, line := range lines {
		m := assertionRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		body = append(body, normalize(m[1]+":"+m[2]+": "+m[3]))
	}
	return body
}

// The normalization rules. ORDER MATTERS between the first and the third: a
// frame offset is written in hex, so +0x1c has to become +0xOFF before the
// general address rule would turn it into +0xADDR and lose the distinction
// between an offset and a pointer.
var (
	reOffset    = regexp.MustCompile(`\+0x[0-9a-fA-F]+`)
	reGoroutine = regexp.MustCompile(`\b([Gg]oroutine) \d+\b`)
	reAddr      = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	reModCache  = regexp.MustCompile(`\S*/pkg/mod/`)
	reGoBuild   = regexp.MustCompile(`go-build\d+`)
	// Durations, including the compound form Go prints for anything over a
	// minute (2m0.5s). The leading \b keeps this out of identifiers and
	// versions: there is no boundary before the 1 in "go1.24.7".
	reDuration = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)(?:\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h))*\b`)
)

func normalize(s string) string {
	s = reOffset.ReplaceAllString(s, "+0xOFF")
	s = reGoroutine.ReplaceAllString(s, "${1} N")
	s = reAddr.ReplaceAllString(s, "0xADDR")
	s = reModCache.ReplaceAllString(s, "MODCACHE/")
	s = reGoBuild.ReplaceAllString(s, "go-build")
	s = reDuration.ReplaceAllString(s, "Ns")
	return s
}
