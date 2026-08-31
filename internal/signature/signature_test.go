package signature

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// lines turns a raw block of failure output into the per-event slice Of takes.
// Splitting on newlines and putting them back is deliberate: it is the shape
// `go test -json` delivers, one line per output event.
func lines(block string) []string {
	block = strings.TrimPrefix(block, "\n")
	var out []string
	for _, l := range strings.SplitAfter(block, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// The same panic, twice. Everything that differs between these two is a class
// this package normalizes: the goroutine ID, the heap addresses, the frame
// offsets, the temporary build directory, and the reported duration.
const panicRunA = `
--- FAIL: TestPanics (0.00s)
panic: assignment to entry in nil map [recovered]
	panic: assignment to entry in nil map

goroutine 6 [running]:
testing.tRunner.func1.2({0x550060, 0x6a2740})
	/tmp/go-build3841927/b001/testing.go:1734 +0x21c
example.com/p.TestPanics(0xc000003500?)
	/home/u/p/crash_test.go:37 +0x28
`

const panicRunB = `
--- FAIL: TestPanics (1.42s)
panic: assignment to entry in nil map [recovered]
	panic: assignment to entry in nil map

goroutine 4711 [running]:
testing.tRunner.func1.2({0x7ffd0088, 0xabcdef12})
	/tmp/go-build99/b001/testing.go:1734 +0x1c
example.com/p.TestPanics(0xc0009f1180?)
	/home/u/p/crash_test.go:37 +0x9ab
`

// A genuinely different cause at the same site.
const panicDifferentCause = `
panic: runtime error: index out of range [3] with length 2

goroutine 6 [running]:
example.com/p.TestPanics(0xc000003500?)
	/home/u/p/crash_test.go:37 +0x28
`

// A deep stack whose frames 4 and 5 differ from its sibling below, so that a
// signature taking three frames cannot tell them apart and one taking five can.
const deepStackA = `
panic: boom

goroutine 7 [running]:
example.com/p.f1()
	/x/a.go:11 +0x11
example.com/p.f2()
	/x/a.go:22 +0x22
example.com/p.f3()
	/x/a.go:33 +0x33
example.com/p.f4Alpha()
	/x/a.go:44 +0x44
example.com/p.f5Alpha()
	/x/a.go:55 +0x55
`

const deepStackB = `
panic: boom

goroutine 7 [running]:
example.com/p.f1()
	/x/a.go:11 +0x11
example.com/p.f2()
	/x/a.go:22 +0x22
example.com/p.f3()
	/x/a.go:33 +0x33
example.com/p.f4Beta()
	/x/a.go:66 +0x66
example.com/p.f5Beta()
	/x/a.go:77 +0x77
`

const raceRunA = `
==================
WARNING: DATA RACE
Read at 0x00c0001922a8 by goroutine 9:
  example.com/p.TestRaces.func1()
      /home/u/p/crash_test.go:52 +0x84

Previous write at 0x00c0001922a8 by goroutine 10:
  example.com/p.TestRaces.func1()
      /home/u/p/crash_test.go:52 +0x96

Goroutine 9 (running) created at:
  example.com/p.TestRaces()
      /home/u/p/crash_test.go:50 +0x84
==================
`

// raceRunB differs from raceRunA only in addresses, goroutine IDs and offsets,
// and it differs in the SECOND stack as well as the first.
const raceRunB = `
==================
WARNING: DATA RACE
Read at 0x00c000abcd00 by goroutine 41:
  example.com/p.TestRaces.func1()
      /home/u/p/crash_test.go:52 +0x1
Previous write at 0x00c000abcd00 by goroutine 77:
  example.com/p.TestRaces.func1()
      /home/u/p/crash_test.go:52 +0x2

Goroutine 41 (running) created at:
  example.com/p.TestRaces()
      /home/u/p/crash_test.go:50 +0x3
==================
`

// raceSecondStackDiffers matches raceRunA in its first stack and names a
// different function in its second.
const raceSecondStackDiffers = `
==================
WARNING: DATA RACE
Read at 0x00c0001922a8 by goroutine 9:
  example.com/p.TestRaces.func1()
      /home/u/p/crash_test.go:52 +0x84

Previous write at 0x00c0001922a8 by goroutine 10:
  example.com/p.someOtherWriter()
      /home/u/p/other.go:14 +0x96
==================
`

// TestSignatureGrouping is the whole contract: which failures share a hash and
// which do not. Every row names the property it pins, because a row that pins
// nothing is a row that cannot fail for a reason anyone would act on.
func TestSignatureGrouping(t *testing.T) {
	tests := []struct {
		name      string
		pins      string
		frames    int
		a, b      string
		wantEqual bool
	}{
		{
			name:      "same panic, different goroutine ID, addresses, offsets, build dir and duration",
			pins:      "every normalization class fires; two runs of one bug are one cluster",
			a:         panicRunA,
			b:         panicRunB,
			wantEqual: true,
		},
		{
			name:      "panics with different messages",
			pins:      "the panic message discriminates; two causes are never merged",
			a:         panicRunA,
			b:         panicDifferentCause,
			wantEqual: false,
		},
		{
			name:      "same panic message, different failing frame",
			pins:      "frames are part of the signature, not decoration",
			a:         deepStackA,
			b:         strings.Replace(deepStackA, "example.com/p.f1()", "example.com/p.gONE()", 1),
			wantEqual: false,
		},
		{
			name:      "frames 4 and 5 differ, three frames kept",
			pins:      "K is consulted: at K=3 the differing frames are outside the signature",
			frames:    3,
			a:         deepStackA,
			b:         deepStackB,
			wantEqual: true,
		},
		{
			name:      "frames 4 and 5 differ, five frames kept",
			pins:      "K is consulted: at K=5 the same pair is inside the signature",
			frames:    5,
			a:         deepStackA,
			b:         deepStackB,
			wantEqual: false,
		},
		{
			name:      "same assertion line, same message",
			pins:      "an assertion signature is file:line plus message",
			a:         "    flaky_test.go:62: global state was poisoned\n--- FAIL: TestOrderDependent (0.00s)\n",
			b:         "    flaky_test.go:62: global state was poisoned\n--- FAIL: TestOrderDependent (1.11s)\n",
			wantEqual: true,
		},
		{
			name:      "same assertion line, different message",
			pins:      "the message discriminates; one line can host two bugs",
			a:         "    flaky_test.go:62: global state was poisoned\n",
			b:         "    flaky_test.go:62: connection refused\n",
			wantEqual: false,
		},
		{
			name:      "same assertion message, different line",
			pins:      "the file:line discriminates; one message can come from two sites",
			a:         "    flaky_test.go:62: boom\n",
			b:         "    flaky_test.go:63: boom\n",
			wantEqual: false,
		},
		{
			name:      "assertion indented as a subtest and as a top-level test",
			pins:      "subtest indentation is not a difference between failures",
			a:         "    flaky_test.go:62: boom\n",
			b:         "            flaky_test.go:62: boom\n",
			wantEqual: true,
		},
		{
			name:      "one assertion versus two at the same first line",
			pins:      "every assertion line is kept; a second failure is not swallowed",
			a:         "    a_test.go:10: first\n",
			b:         "    a_test.go:10: first\n    a_test.go:20: second\n",
			wantEqual: false,
		},
		{
			name:      "race reports differing only in addresses and goroutine IDs, in both stacks",
			pins:      "the SECOND race stack is normalized too, not just the first",
			a:         raceRunA,
			b:         raceRunB,
			wantEqual: true,
		},
		{
			name:      "race reports whose second stack names a different function",
			pins:      "the second race stack is INCLUDED; without this the row above would pass by omission",
			a:         raceRunA,
			b:         raceSecondStackDiffers,
			wantEqual: false,
		},
		{
			name: "go test timeout panics differing only in the elapsed time",
			pins: "durations are normalized; a timeout that fired at 10m0s and one at " +
				"10m0.5s are the same bug",
			a:         "panic: test timed out after 10m0s\n\ngoroutine 1 [running]:\nx.F()\n\t/x/a.go:9 +0x1\n",
			b:         "panic: test timed out after 10m0.5s\n\ngoroutine 1 [running]:\nx.F()\n\t/x/a.go:9 +0x1\n",
			wantEqual: true,
		},
		{
			name:      "module cache paths under different GOPATHs",
			pins:      "the module cache prefix is normalized",
			a:         "panic: boom\n\ngoroutine 1 [running]:\nx.F()\n\t/root/go/pkg/mod/example.com/d@v1.2.3/f.go:9 +0x1\n",
			b:         "panic: boom\n\ngoroutine 1 [running]:\nx.F()\n\t/home/ci/gopath/pkg/mod/example.com/d@v1.2.3/f.go:9 +0x2\n",
			wantEqual: true,
		},
		{
			name:      "an assertion and a panic with identical text",
			pins:      "the kind is part of the hash, so shapes cannot collide",
			a:         "    a_test.go:10: boom\n",
			b:         "panic: boom\n\ngoroutine 1 [running]:\nx.F()\n\t/x/a.go:10 +0x1\n",
			wantEqual: false,
		},
		{
			name:      "two empty outputs",
			pins:      "an empty failure has a signature rather than a crash",
			a:         "",
			b:         "",
			wantEqual: true,
		},
		{
			name:      "two different unparseable outputs",
			pins:      "unrecognised output still discriminates rather than collapsing to one cluster",
			a:         "signal: killed\n",
			b:         "exit status 2\n",
			wantEqual: false,
		},
		{
			name:      "the same unparseable output twice",
			pins:      "unrecognised output is stable, not merely different every time",
			a:         "signal: killed\n",
			b:         "signal: killed\n",
			wantEqual: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frames := tc.frames
			if frames == 0 {
				frames = Frames
			}
			a := OfFrames(lines(tc.a), frames)
			b := OfFrames(lines(tc.b), frames)
			if got := a.Hash == b.Hash; got != tc.wantEqual {
				t.Errorf("hashes equal = %v, want %v; this row pins that %s\n\nA (%s):\n%s\n\nB (%s):\n%s",
					got, tc.wantEqual, tc.pins, a.Hash, a.Normalized, b.Hash, b.Normalized)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		pins string
		in   string
		want Kind
	}{
		{
			name: "t.Fatalf output",
			pins: "the majority shape - a failure with no stack at all - is recognised",
			in:   "    flaky_test.go:62: boom\n",
			want: KindAssertion,
		},
		{
			name: "a panic with a stack",
			pins: "a message followed by goroutine stacks is a panic",
			in:   panicRunA,
			want: KindPanic,
		},
		{
			name: "a fatal runtime error",
			pins: "fatal error is the same shape as panic and must not fall through to unknown",
			in:   "fatal error: all goroutines are asleep - deadlock!\n\ngoroutine 1 [chan receive]:\nx.F()\n\t/x/a.go:9 +0x1\n",
			want: KindPanic,
		},
		{
			name: "a race report",
			pins: "race is checked before assertion; its own testing.go:NNNN line must not win",
			in:   raceRunA + "    testing.go:1490: race detected during execution of test\n",
			want: KindRace,
		},
		{
			name: "nothing recognisable",
			pins: "unrecognised output is named as such rather than forced into a shape",
			in:   "signal: killed\n",
			want: KindUnknown,
		},
		{
			name: "no output at all",
			pins: "an empty failure classifies without indexing off the end of anything",
			in:   "",
			want: KindUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Of(lines(tc.in)).Kind; got != tc.want {
				t.Errorf("Kind = %v, want %v; this row pins that %s", got, tc.want, tc.pins)
			}
		})
	}
}

var hashRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestHashShape pins the identifier's form. It appears in user-facing output
// and in the JSON report, where it becomes a compatibility surface at v1.0.0.
func TestHashShape(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "assertion", in: "    a_test.go:1: boom\n"},
		{name: "panic", in: panicRunA},
		{name: "race", in: raceRunA},
		{name: "unknown", in: "signal: killed\n"},
		{name: "empty", in: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sig := Of(lines(tc.in))
			if !hashRE.MatchString(sig.Hash) {
				t.Errorf("Hash = %q, want 16 lowercase hex characters", sig.Hash)
			}
			if again := Of(lines(tc.in)).Hash; again != sig.Hash {
				t.Errorf("Hash is not stable across calls: %q then %q", sig.Hash, again)
			}
		})
	}
}

// TestFramesClamped: a negative frame count keeps no frames rather than
// silently keeping all of them, which would make two unrelated deep failures
// look distinct for reasons the caller did not ask for.
func TestFramesClamped(t *testing.T) {
	none := OfFrames(lines(deepStackA), 0)
	negative := OfFrames(lines(deepStackA), -1)
	if none.Hash != negative.Hash {
		t.Errorf("frames=-1 hash %q != frames=0 hash %q", negative.Hash, none.Hash)
	}
	if strings.Contains(none.Normalized, "a.go") {
		t.Errorf("frames=0 kept a frame:\n%s", none.Normalized)
	}
	if !strings.Contains(none.Normalized, "panic: boom") {
		t.Errorf("frames=0 dropped the message:\n%s", none.Normalized)
	}
}

// ---------------------------------------------------------------------------
// Against the recorded streams
// ---------------------------------------------------------------------------

const fixturePkg = "github.com/AarinB1/flakescope/testdata/flakypkg"

// testOutput pulls one test's output lines out of a recorded stream. The
// streams are replayed rather than produced, so nothing here invokes `go test`
// (CLAUDE.md rule 2).
func testOutput(t *testing.T, stream, test string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "streams", stream))
	if err != nil {
		t.Fatalf("reading %s: %v", stream, err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev struct {
			Action string
			Test   string
			Output string
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // a truncated final object; the events before it stand
		}
		if ev.Action == "output" && ev.Test == test {
			out = append(out, ev.Output)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no output for %s in %s", test, stream)
	}
	return out
}

// TestRecordedStreams is the exit criterion measured against real recordings
// rather than hand-written text: two textually different failures with the same
// cause land in one cluster, and two different causes do not.
func TestRecordedStreams(t *testing.T) {
	panicP1 := testOutput(t, "panic1.json", "TestPanics")
	panicP4 := testOutput(t, "panic4.json", "TestPanics")
	loadP2 := testOutput(t, "loadfail2.json", "TestLoadDependent")
	loadP4 := testOutput(t, "loadfail.json", "TestLoadDependent")
	brokenP2 := testOutput(t, "loadfail2.json", "TestAlwaysFails")
	brokenP4 := testOutput(t, "loadfail.json", "TestAlwaysFails")
	order := testOutput(t, "orderfail.json", "TestOrderDependent")
	race := testOutput(t, "race.json", "TestRaces")

	tests := []struct {
		name      string
		pins      string
		a, b      []string
		wantEqual bool
	}{
		{
			name:      "one panic recorded under GOMAXPROCS=1 and GOMAXPROCS=4",
			pins:      "the exit criterion: textually different, same cause, one cluster",
			a:         panicP1,
			b:         panicP4,
			wantEqual: true,
		},
		{
			name:      "the always-broken test under two configurations",
			pins:      "an assertion that does not mention its configuration is one cluster",
			a:         brokenP2,
			b:         brokenP4,
			wantEqual: true,
		},
		{
			name:      "the always-broken test and the order-dependent one",
			pins:      "the exit criterion: two different causes, two clusters",
			a:         brokenP4,
			b:         order,
			wantEqual: false,
		},
		{
			name:      "a panic and an assertion",
			pins:      "the two shapes never merge",
			a:         panicP1,
			b:         order,
			wantEqual: false,
		},
		{
			name: "the load-dependent test under GOMAXPROCS=2 and GOMAXPROCS=4",
			pins: "PREFER SPLITTING: one bug, two clusters, because its message names " +
				"the configuration and integers in messages are deliberately not normalized",
			a:         loadP2,
			b:         loadP4,
			wantEqual: false,
		},
		{
			name:      "a race report against itself",
			pins:      "the recorded race layout parses; it is not silently KindUnknown",
			a:         race,
			b:         race,
			wantEqual: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := Of(tc.a), Of(tc.b)
			if got := a.Hash == b.Hash; got != tc.wantEqual {
				t.Errorf("hashes equal = %v, want %v; this row pins that %s\n\nA (%s):\n%s\n\nB (%s):\n%s",
					got, tc.wantEqual, tc.pins, a.Hash, a.Normalized, b.Hash, b.Normalized)
			}
		})
	}

	// The recorded race must classify as a race. Without this the row above
	// would pass with both sides falling through to KindUnknown.
	if got := Of(race).Kind; got != KindRace {
		t.Errorf("recorded race report classified as %v, want race:\n%s", got, Of(race).Normalized)
	}
	if got := Of(panicP1).Kind; got != KindPanic {
		t.Errorf("recorded panic classified as %v, want panic:\n%s", got, Of(panicP1).Normalized)
	}
	if got := Of(order).Kind; got != KindAssertion {
		t.Errorf("recorded assertion classified as %v, want assertion:\n%s", got, Of(order).Normalized)
	}
}

// TestNormalizedTellsOffsetsFromPointers pins the one thing the frame-offset
// rule actually decides.
//
// It decides NOTHING about grouping: the general address rule matches +0x1c as
// readily as it matches a pointer, so with the offset rule removed two failures
// that differ only in their offsets still land in the same cluster. What the
// rule decides is whether Normalized - which is what a person reads when two
// signatures did not match and they want to know why - distinguishes a frame
// offset from a heap address. That is a small claim, and this is the assertion
// sized to it rather than a grouping assertion that could not fail.
func TestNormalizedTellsOffsetsFromPointers(t *testing.T) {
	sig := Of(lines(panicRunA))
	if !strings.Contains(sig.Normalized, "+0xOFF") {
		t.Errorf("no frame offset rendered as +0xOFF; offsets read as pointers:\n%s", sig.Normalized)
	}
	if strings.Contains(sig.Normalized, "+0xADDR") {
		t.Errorf("a frame offset was rendered as a pointer address:\n%s", sig.Normalized)
	}
	if !strings.Contains(sig.Normalized, "({0xADDR, 0xADDR})") {
		t.Errorf("no pointer rendered as 0xADDR:\n%s", sig.Normalized)
	}
}
