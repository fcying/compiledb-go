package internal

import (
	"testing"
)

func TestNormalizeEncoding(t *testing.T) {
	encoding, err := NormalizeEncoding("")
	if err != nil {
		t.Fatalf("expected default encoding, got error: %v", err)
	}
	if encoding != EncodingRaw {
		t.Fatalf("expected default encoding %q, got %q", EncodingRaw, encoding)
	}

	encoding, err = NormalizeEncoding("GB18030")
	if err != nil {
		t.Fatalf("expected gb18030 to be accepted, got error: %v", err)
	}
	if encoding != EncodingGB18030 {
		t.Fatalf("expected normalized encoding %q, got %q", EncodingGB18030, encoding)
	}

	if _, err := NormalizeEncoding("latin1"); err == nil {
		t.Fatal("expected unsupported encoding error")
	}
}

func TestShellJoinArgs(t *testing.T) {
	got := ShellJoinArgs([]string{"clang", "-DNAME=hello world", "-c", "src dir/a.c", "-DQUOTE=it's"})
	want := `clang '-DNAME=hello world' -c 'src dir/a.c' '-DQUOTE=it'"'"'s'`

	if got != want {
		t.Fatalf("unexpected shell join output\nwant: %q\ngot:  %q", want, got)
	}
}

func TestShellJoinArgsQuotesShellExpansionCharacters(t *testing.T) {
	got := ShellJoinArgs([]string{"gcc", "#define", "~root", "$HOME", "*.c"})
	want := `gcc '#define' '~root' '$HOME' '*.c'`
	if got != want {
		t.Fatalf("unexpected shell join output\nwant: %q\ngot:  %q", want, got)
	}
}

func TestWindowsQuoteArgument(t *testing.T) {
	for argument, want := range map[string]string{
		`plain`:                `plain`,
		`C:\Program Files\gcc`: `"C:\Program Files\gcc"`,
		`value\`:               `value\`,
		`a"b`:                  `"a\"b"`,
	} {
		if got := windowsQuoteArgument(argument); got != want {
			t.Fatalf("unexpected Windows quoting for %q: want %q, got %q", argument, want, got)
		}
	}
}
