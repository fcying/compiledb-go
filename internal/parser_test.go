package internal

import (
	"regexp"
	"testing"
)

func TestSplitArgsPreservesQuotedSpaces(t *testing.T) {
	args := splitArgs(`gcc -c test.c -isystem "/tmp/include dir" -DNAME="hello world"`)

	expected := []string{"gcc", "-c", "test.c", "-isystem", "/tmp/include dir", "-DNAME=hello world"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %#v", len(expected), len(args), args)
	}

	for i := range expected {
		if args[i] != expected[i] {
			t.Fatalf("arg[%d] expected %q, got %q", i, expected[i], args[i])
		}
	}
}

func TestProcessCompileCommandMacrosWithSpacePath(t *testing.T) {
	compileRegex = regexp.MustCompile(RegexCompile)
	fileRegex = regexp.MustCompile(RegexFile)

	ParseConfig = Config{
		Macros:   `-DDEBUG=1 -isystem "/tmp/include dir"`,
		NoStrict: true,
	}

	args, filePath := processCompileCommand(`gcc -c test.c`, "/tmp")
	if filePath != "test.c" {
		t.Fatalf("expected filePath %q, got %q", "test.c", filePath)
	}

	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-isystem" {
			found = true
			if args[i+1] != "/tmp/include dir" {
				t.Fatalf("expected -isystem path to preserve spaces, got %q", args[i+1])
			}
			break
		}
	}

	if !found {
		t.Fatalf("expected -isystem to be present in args: %#v", args)
	}
}
