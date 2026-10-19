package errfp

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeMessage(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"posix path", "open /home/alice/.config/tool/config.yaml: permission denied", "open <path>: permission denied"},
		{"home tilde", "cannot read ~/notes.txt", "cannot read <path>"},
		{"temp dir", "mkdir /tmp/go-build1234/b001: file exists", "mkdir <path>: file exists"},
		{"windows path", `open C:\Users\Bob\AppData\Local\Temp\x.json: access denied`, "open <path>: access denied"},
		{"unc path", `cannot reach \\fileserver\share\x`, "cannot reach <path>"},
		{"relative path kept", "invalid read/write mode", "invalid read/write mode"},
		{"quoted string", `unknown flag "--frobnicate"`, "unknown flag <str>"},
		{"single quoted path", "open '/Users/carol/x': no such file", "open <str>: no such file"},
		{"uuid", "project 3f8c2a1e-9b4d-4c6e-8f0a-1b2c3d4e5f60 not found", "project <uuid> not found"},
		{"hex id", "commit a1b2c3d4e5f6 missing", "commit <hex> missing"},
		{"pointer", "nil map at 0xc000123456", "nil map at <hex>"},
		{"long word kept", "deadbeefcafe is a word here", "deadbeefcafe is a word here"},
		{"numbers", "retry 3 of 5 failed after 30s", "retry <n> of <n> failed after <n>"},
		{"ip and port", "dial tcp 10.0.0.12:8443: connection refused", "dial tcp <n>: connection refused"},
		{"identifier digits kept", "utf8 decode error in ipv6 parser", "utf8 decode error in ipv6 parser"},
		{"url", "GET https://api.example.com/v1/users/42?x=1 returned 503", "GET <url> returned <n>"},
		{"whitespace", "  too   many\n spaces ", "too many spaces"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeMessage(c.in); got != c.want {
				t.Errorf("NormalizeMessage(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFrames(t *testing.T) {
	cases := []struct {
		name, stack string
		want        []string
	}{
		{
			name: "go panic",
			stack: `panic: runtime error: index out of range [5] with length 3

goroutine 1 [running]:
github.com/acme/tool/internal/sync.(*Client).Pull(0xc000010000, {0x1234, 0x5})
	/home/alice/go/pkg/mod/github.com/acme/tool@v1.2.3/internal/sync/client.go:88 +0x1d
main.main()
	/tmp/build-xyz/main.go:12 +0x25
created by main.start in goroutine 1
	/tmp/build-xyz/main.go:30 +0x40`,
			want: []string{
				"github.com/acme/tool/internal/sync.(*Client).Pull client.go",
				"main.main main.go",
				"main.start main.go",
			},
		},
		{
			name: "python traceback is innermost last",
			stack: `Traceback (most recent call last):
  File "/home/bob/.local/bin/tool", line 8, in <module>
    sys.exit(main())
  File "/home/bob/.local/lib/python3.12/site-packages/tool/cli.py", line 42, in main
    run(args)
  File "/home/bob/.local/lib/python3.12/site-packages/tool/run.py", line 7, in run
    raise ValueError("bad")
ValueError: bad`,
			want: []string{"run run.py", "main cli.py", "<module> tool"},
		},
		{
			name: "node",
			stack: `Error: ENOENT: no such file or directory, open '/Users/carol/project/.toolrc'
    at Object.openSync (node:fs:601:3)
    at readConfig (/Users/carol/.npm/_npx/1a2b3c4d/node_modules/tool/dist/config.js:10:15)
    at file:///Users/carol/.npm/_npx/1a2b3c4d/node_modules/tool/dist/chunk-3f2a9b1c.mjs:1:2
    at async Promise.all (index 0)`,
			want: []string{
				"Object.openSync node:fs",
				"readConfig config.js",
				"chunk-<hex>.mjs",
				"async Promise.all (index <n>)",
			},
		},
		{
			name: "java",
			stack: `java.lang.IllegalStateException: boom
	at com.acme.tool.Sync.pull(Sync.java:120)
	at com.acme.tool.Main.main(Main.java:15)
	at java.base/jdk.internal.reflect.NativeMethodAccessorImpl.invoke0(Native Method)`,
			want: []string{
				"com.acme.tool.Sync.pull Sync.java",
				"com.acme.tool.Main.main Main.java",
				"java.base/jdk.internal.reflect.NativeMethodAccessorImpl.invoke0(Native Method)",
			},
		},
		{
			name: "ruby",
			stack: `/home/dan/.gem/ruby/3.3.0/gems/tool-1.0/lib/tool/sync.rb:12:in 'Tool::Sync#pull'
/home/dan/.gem/ruby/3.3.0/gems/tool-1.0/exe/tool:5:in '<main>'`,
			want: []string{"Tool::Sync#pull sync.rb"},
		},
		{
			name:  "windows paths reduce to the file name",
			stack: `   at Acme.Tool.Sync.Pull() in C:\Users\Eve\src\tool\Sync.cs:line 40` + "\n" + `   at Acme.Tool.Program.Main(String[] args) in C:\Users\Eve\src\tool\Program.cs:line 9`,
			want: []string{
				"Acme.Tool.Sync.Pull() Sync.cs",
				"Acme.Tool.Program.Main(String[] args) Program.cs",
			},
		},
		{name: "no frames", stack: "something went wrong", want: nil},
		{name: "empty", stack: "", want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Frames(c.stack); !slices.Equal(got, c.want) {
				t.Errorf("Frames() =\n  %q\nwant\n  %q", got, c.want)
			}
		})
	}
}

func TestFramesKeepsInnermost(t *testing.T) {
	var b strings.Builder
	for _, fn := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		b.WriteString("    at " + fn + " (/srv/app/" + fn + ".js:1:1)\n")
	}
	got := Frames(b.String())
	want := []string{"a a.js", "b b.js", "c c.js", "d d.js", "e e.js"}
	if !slices.Equal(got, want) {
		t.Errorf("Frames() = %q, want %q", got, want)
	}
}

func TestComputeIgnoresMachineSpecifics(t *testing.T) {
	stack := func(home string, line int) string {
		return "main.run()\n\t" + home + "/src/tool/run.go:" + strings.Repeat("1", line) + " +0x1d\nmain.main()\n\t" + home + "/src/tool/main.go:9 +0x25"
	}
	a, okA := Compute("open /home/alice/x: denied", stack("/home/alice", 2))
	b, okB := Compute("open /Users/bob/y: denied", stack("/Users/bob", 3))
	if !okA || !okB {
		t.Fatal("Compute: want ok for both")
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("fingerprints differ across machines: %s vs %s", a.Fingerprint, b.Fingerprint)
	}
	if a.Source != SourceStack || a.TopFrame != "main.run run.go" {
		t.Errorf("got source %q top frame %q", a.Source, a.TopFrame)
	}
	if len(a.Fingerprint) != 16 {
		t.Errorf("fingerprint %q: want 16 hex digits", a.Fingerprint)
	}

	other, _ := Compute("open /home/alice/x: denied", "main.other()\n\t/home/alice/src/tool/other.go:3 +0x1")
	if other.Fingerprint == a.Fingerprint {
		t.Error("different stacks share a fingerprint")
	}
}

func TestComputeMissingStackFallsBackToMessage(t *testing.T) {
	a, ok := Compute("timeout after 30s talking to 10.0.0.1:443", "")
	if !ok {
		t.Fatal("Compute: want ok")
	}
	b, _ := Compute("timeout after 5s talking to 192.168.1.9:8443", "not a stack")
	if a.Source != SourceMessage || a.TopFrame != "" {
		t.Errorf("got source %q top frame %q, want message-only", a.Source, a.TopFrame)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("same message shape, different fingerprints: %s vs %s", a.Fingerprint, b.Fingerprint)
	}
	// The same normalized text as a message and as a stack must not collide.
	if s, _ := Compute("", "at x (/a/x.js:1:1)"); s.Fingerprint == a.Fingerprint {
		t.Error("stack and message fingerprints collide")
	}
}

func TestComputeNothingToGroupOn(t *testing.T) {
	if _, ok := Compute("", ""); ok {
		t.Error("Compute(\"\", \"\"): want !ok")
	}
	if _, ok := Compute("   ", "\n\n"); ok {
		t.Error("Compute(blank): want !ok")
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("héllo", 10); got != "héllo" {
		t.Errorf("short: got %q", got)
	}
	if got := Truncate("héllo world", 5); got != "héll…" {
		t.Errorf("long: got %q", got)
	}
}
