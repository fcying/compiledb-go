package internal

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitGNUResponseFile(t *testing.T) {
	input := []byte("-Iinclude 'source dir/main.c' \"-DNAME=hello\\ world\" escaped\\ value '#literal' \"quoted\\\"value\" \"-DDOLLAR=\\$HOME\" \"-DTOOL=\\`name\\`\" '-DSINGLE=a\\ b' \"\" '' a\"\"b -I \"\"")
	want := []string{"-Iinclude", "source dir/main.c", "-DNAME=hello world", "escaped value", "#literal", `quoted"value`, "-DDOLLAR=$HOME", "-DTOOL=`name`", "-DSINGLE=a b", "", "", "ab", "-I", ""}

	got, parseErr := splitGNUResponseFile(input, maxResponseFileArguments)
	if parseErr != nil {
		t.Fatalf("parse response file failed: %v", parseErr)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected response arguments:\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestSplitGNUResponseFileRejectsNonportableInput(t *testing.T) {
	for name, test := range map[string]struct {
		input  []byte
		reason string
	}{
		"UTF-8 BOM":                     {input: []byte{0xef, 0xbb, 0xbf, 'x'}, reason: "byte order mark"},
		"UTF-16 BOM":                    {input: []byte{0xff, 0xfe, 'x', 0}, reason: "byte order mark"},
		"NUL":                           {input: []byte{'a', 0, 'b'}, reason: "NUL byte"},
		"invalid UTF-8":                 {input: []byte{0xff}, reason: "invalid UTF-8"},
		"trailing escape":               {input: []byte(`value\`), reason: "trailing escape"},
		"escaped newline":               {input: []byte("value\\\nnext"), reason: "escaped newline"},
		"single quoted trailing escape": {input: []byte("'value\\"), reason: "trailing escape"},
		"double quoted trailing escape": {input: []byte("\"value\\"), reason: "trailing escape"},
		"unterminated quote":            {input: []byte(`'value`), reason: "unterminated quote"},
	} {
		t.Run(name, func(t *testing.T) {
			arguments, parseErr := splitGNUResponseFile(test.input, maxResponseFileArguments)
			if parseErr == nil || !strings.Contains(parseErr.reason, test.reason) {
				t.Fatalf("expected %q error, got arguments=%#v error=%#v", test.reason, arguments, parseErr)
			}
			if parseErr.offset < 0 {
				t.Fatalf("response parse error lacks byte offset: %#v", parseErr)
			}
		})
	}
}

func TestExpandCompilerResponseFilesRecursively(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "outer.rsp"), []byte(`"first value" @@nested.rsp -c src/main.c`), 0o644); err != nil {
		t.Fatalf("write outer response file failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, "@nested.rsp"), []byte(`-DVALUE=one\ two -Iinclude`), 0o644); err != nil {
		t.Fatalf("write nested response file failed: %v", err)
	}

	tool := newTestTool(t, Config{})
	arguments := []string{"ccache", "gcc", "@outer.rsp"}
	got, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
	if err != nil {
		t.Fatalf("expand response files failed: %v", err)
	}
	want := []string{"ccache", "gcc", "first value", "-DVALUE=one two", "-Iinclude", "-c", "src/main.c"}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected expanded arguments:\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestExpandCompilerResponseFilesUsesCompilerWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	workingDir := filepath.Join(root, "build")
	if err := os.Mkdir(workingDir, 0o755); err != nil {
		t.Fatalf("create compiler working directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, "outer.rsp"), []byte(`@nested.rsp`), 0o644); err != nil {
		t.Fatalf("write outer response file failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, "nested.rsp"), []byte(`-c main.c`), 0o644); err != nil {
		t.Fatalf("write nested response file failed: %v", err)
	}

	tool := newTestTool(t, Config{})
	arguments := []string{"gcc", "@outer.rsp"}
	got, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
	if err != nil {
		t.Fatalf("expand response files failed: %v", err)
	}
	if want := []string{"gcc", "-c", "main.c"}; !slices.Equal(got, want) {
		t.Fatalf("nested response path did not use compiler cwd: want %v, got %v", want, got)
	}
}

func TestExpandCompilerResponseFilesRejectsInvalidFiles(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, string){
		"missing": func(_ *testing.T, _ string) {},
		"directory": func(t *testing.T, workingDir string) {
			if err := os.Mkdir(filepath.Join(workingDir, "arguments.rsp"), 0o755); err != nil {
				t.Fatalf("create response directory failed: %v", err)
			}
		},
		"cycle": func(t *testing.T, workingDir string) {
			if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("@arguments.rsp"), 0o644); err != nil {
				t.Fatalf("write cyclic response file failed: %v", err)
			}
		},
		"malformed": func(t *testing.T, workingDir string) {
			if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("-DSECRET=value 'unterminated"), 0o644); err != nil {
				t.Fatalf("write malformed response file failed: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			workingDir := t.TempDir()
			setup(t, workingDir)
			tool := newTestTool(t, Config{})
			arguments := []string{"gcc", "@arguments.rsp", "-c", "outside.c"}
			expanded, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
			if err == nil || expanded != nil {
				t.Fatalf("invalid response file was partially expanded: arguments=%#v error=%v", expanded, err)
			}
			if name == "malformed" && (strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "at byte")) {
				t.Fatalf("malformed response diagnostic leaked contents or lacked offset: %v", err)
			}
		})
	}
}

func TestExpandCompilerResponseFilesEnforcesLimits(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		workingDir := t.TempDir()
		for i := 0; i <= maxResponseFileDepth; i++ {
			contents := "-c main.c"
			if i < maxResponseFileDepth {
				contents = "@" + filepath.Base(filepath.Join(workingDir, responseTestFilename(i+1)))
			}
			if err := os.WriteFile(filepath.Join(workingDir, responseTestFilename(i)), []byte(contents), 0o644); err != nil {
				t.Fatalf("write response file %d failed: %v", i, err)
			}
		}
		assertResponseExpansionError(t, workingDir, "@"+responseTestFilename(0), "depth limit")
	})

	t.Run("file size", func(t *testing.T) {
		workingDir := t.TempDir()
		data := make([]byte, maxResponseFileSize+1)
		for i := range data {
			data[i] = 'x'
		}
		if err := os.WriteFile(filepath.Join(workingDir, "large.rsp"), data, 0o644); err != nil {
			t.Fatalf("write large response file failed: %v", err)
		}
		assertResponseExpansionError(t, workingDir, "@large.rsp", "size limit")
	})

	t.Run("argument count", func(t *testing.T) {
		workingDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workingDir, "many.rsp"), []byte(strings.Repeat("x ", maxResponseFileArguments+1)), 0o644); err != nil {
			t.Fatalf("write response file failed: %v", err)
		}
		assertResponseExpansionError(t, workingDir, "@many.rsp", "argument limit")
	})

	t.Run("nested argument count", func(t *testing.T) {
		workingDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workingDir, "outer.rsp"), []byte("first @nested.rsp"), 0o644); err != nil {
			t.Fatalf("write outer response file failed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(workingDir, "nested.rsp"), []byte(strings.Repeat("x ", maxResponseFileArguments-1)), 0o644); err != nil {
			t.Fatalf("write nested response file failed: %v", err)
		}
		assertResponseExpansionError(t, workingDir, "@outer.rsp", "argument limit")
	})
}

func responseTestFilename(index int) string {
	return "depth-" + string(rune('a'+index)) + ".rsp"
}

func assertResponseExpansionError(t *testing.T, workingDir, responseArgument, reason string) {
	t.Helper()
	tool := newTestTool(t, Config{})
	arguments := []string{"gcc", responseArgument}
	result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
	if err == nil || !strings.Contains(err.Error(), reason) || result != nil {
		t.Fatalf("expected %q expansion failure, got arguments=%#v error=%v", reason, result, err)
	}
}

func TestExpandCompilerResponseFilesHonorsContextCancellation(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("-c main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := newTestTool(t, Config{})
	tool.Context = ctx
	arguments := []string{"gcc", "@arguments.rsp"}
	if result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir); err == nil || result != nil {
		t.Fatalf("canceled expansion succeeded: arguments=%#v error=%v", result, err)
	}
}

func TestResponseFileCompilerModes(t *testing.T) {
	for name, test := range map[string]struct {
		arguments []string
		want      bool
	}{
		"GCC":                   {arguments: []string{"gcc", "@args.rsp"}, want: true},
		"MinGW GCC":             {arguments: []string{"x86_64-w64-mingw32-g++", "@args.rsp"}, want: true},
		"Clang GNU":             {arguments: []string{"clang", "@args.rsp"}, want: true},
		"Clang POSIX quoting":   {arguments: []string{"clang", "--rsp-quoting=posix", "@args.rsp"}, want: true},
		"clang-cl":              {arguments: []string{"clang-cl", "@args.rsp"}},
		"Clang CL mode":         {arguments: []string{"clang", "--driver-mode=cl", "@args.rsp"}},
		"Clang Windows quoting": {arguments: []string{"clang", "--rsp-quoting=windows", "@args.rsp"}},
		"custom compiler":       {arguments: []string{"mycc", "@args.rsp"}},
	} {
		t.Run(name, func(t *testing.T) {
			invocation := parseCompilerInvocation(test.arguments)
			if got := supportsGNUResponseFiles(test.arguments, invocation); got != test.want {
				t.Fatalf("unexpected GNU response support: want %t, got %t", test.want, got)
			}
		})
	}
}

func TestExpandCompilerResponseFilesRejectsCLModeFromResponseFile(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("--driver-mode=cl -c main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	tool := newTestTool(t, Config{})
	arguments := []string{"clang", "@arguments.rsp"}
	result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
	if err == nil || !strings.Contains(err.Error(), "CL driver mode") || result != nil {
		t.Fatalf("CL mode from response file was accepted: arguments=%#v error=%v", result, err)
	}
}

func TestExpandCompilerResponseFilesUsesOuterResponseQuoting(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("--rsp-quoting=windows -c main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	tool := newTestTool(t, Config{})

	t.Run("final outer POSIX", func(t *testing.T) {
		arguments := []string{"clang", "--rsp-quoting=windows", "--rsp-quoting=posix", "@arguments.rsp"}
		result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
		if err != nil {
			t.Fatalf("final outer POSIX response quoting was rejected: %v", err)
		}
		want := []string{"clang", "--rsp-quoting=windows", "--rsp-quoting=posix", "--rsp-quoting=windows", "-c", "main.c"}
		if !slices.Equal(result, want) {
			t.Fatalf("unexpected expanded arguments:\nwant: %#v\ngot:  %#v", want, result)
		}
	})

	t.Run("final outer Windows", func(t *testing.T) {
		arguments := []string{"clang", "--rsp-quoting=posix", "--rsp-quoting=windows", "@arguments.rsp"}
		result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
		if err != nil {
			t.Fatalf("opaque Windows response quoting returned an error: %v", err)
		}
		if !slices.Equal(result, arguments) {
			t.Fatalf("Windows-quoted response file was expanded:\nwant: %#v\ngot:  %#v", arguments, result)
		}
	})

	t.Run("response contents do not change tokenizer", func(t *testing.T) {
		arguments := []string{"clang", "--rsp-quoting=posix", "@arguments.rsp"}
		result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
		if err != nil {
			t.Fatalf("response-file option changed the outer tokenizer: %v", err)
		}
		want := []string{"clang", "--rsp-quoting=posix", "--rsp-quoting=windows", "-c", "main.c"}
		if !slices.Equal(result, want) {
			t.Fatalf("unexpected expanded arguments:\nwant: %#v\ngot:  %#v", want, result)
		}
	})
}
