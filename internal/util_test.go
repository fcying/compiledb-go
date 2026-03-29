package internal

import (
	"bytes"
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

func TestTransferPrintRaw(t *testing.T) {
	var out bytes.Buffer
	TransferPrint(bytes.NewBufferString("hello\n"), &out, EncodingRaw)

	if out.String() != "hello\n" {
		t.Fatalf("expected raw output to pass through unchanged, got %q", out.String())
	}
}

func TestTransferPrintGB18030(t *testing.T) {
	var out bytes.Buffer
	TransferPrint(bytes.NewBuffer([]byte{0xC4, 0xE3, 0xBA, 0xC3, '\n'}), &out, EncodingGB18030)

	if out.String() != "你好\n" {
		t.Fatalf("expected gb18030-decoded output, got %q", out.String())
	}
}

func TestShellJoinArgs(t *testing.T) {
	got := ShellJoinArgs([]string{"clang", "-DNAME=hello world", "-c", "src dir/a.c", "-DQUOTE=it's"})
	want := `clang '-DNAME=hello world' -c 'src dir/a.c' '-DQUOTE=it'"'"'s'`

	if got != want {
		t.Fatalf("unexpected shell join output\nwant: %q\ngot:  %q", want, got)
	}
}
