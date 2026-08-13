package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCompilerArgumentIndex(t *testing.T) {
	for name, test := range map[string]struct {
		arguments []string
		want      int
	}{
		"compiler":        {arguments: []string{"gcc", "-c", "main.c"}, want: 0},
		"ccache":          {arguments: []string{"ccache", "gcc", "-c", "main.c"}, want: 1},
		"ccache config":   {arguments: []string{"ccache", "compiler_check=content", "gcc", "-c", "main.c"}, want: 2},
		"nested wrappers": {arguments: []string{"sccache.exe", "distcc", "clang", "-c", "main.c"}, want: 2},
		"icecc compiler":  {arguments: []string{"icecc", "g++", "-c", "main.cpp"}, want: 1},
		"implicit distcc": {arguments: []string{"distcc", "-c", "main.c"}, want: 1},
		"implicit icecc":  {arguments: []string{"icecc", "-c", "main.c"}, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := compilerArgumentIndex(test.arguments); got != test.want {
				t.Fatalf("unexpected compiler index: want %d, got %d", test.want, got)
			}
		})
	}
}

func TestCompilerInvocationRejectsWrapperManagementCommands(t *testing.T) {
	for _, arguments := range [][]string{
		{"ccache", "-c", "main.c"},
		{"ccache", "--", "gcc", "-c", "main.c"},
		{"sccache", "--show-stats"},
	} {
		if invocation := parseCompilerInvocation(arguments); invocation.valid {
			t.Fatalf("wrapper management command was treated as a compiler invocation: %v", arguments)
		}
	}
}

func TestCompilerInvocationDisablesUnsafeCCacheOverrides(t *testing.T) {
	for _, arguments := range [][]string{
		{"ccache", "compiler=clang", "gcc", "-c", "main.c"},
		{"ccache", "prefix_command=distcc", "gcc", "-c", "main.c"},
	} {
		invocation := parseCompilerInvocation(arguments)
		if !invocation.valid || invocation.probeDisabled == "" {
			t.Fatalf("ccache override did not disable probing: %#v", invocation)
		}
	}

	t.Setenv("CCACHE_COMPILER", "clang")
	invocation := parseCompilerInvocation([]string{"ccache", "gcc", "-c", "main.c"})
	if !invocation.valid || invocation.probeDisabled == "" {
		t.Fatalf("CCACHE_COMPILER did not disable probing: %#v", invocation)
	}
}

func TestCompilerLanguage(t *testing.T) {
	for name, test := range map[string]struct {
		arguments []string
		file      string
		want      string
	}{
		"default C":                {arguments: []string{"gcc", "-c", "main.c"}, file: "main.c", want: "c"},
		"C++ extension":            {arguments: []string{"gcc", "-c", "main.cpp"}, file: "main.cpp", want: "c++"},
		"C++ driver":               {arguments: []string{"g++", "-c", "main.c"}, file: "main.c", want: "c++"},
		"standard keeps language":  {arguments: []string{"gcc", "-std=gnu++20", "-c", "main.c"}, file: "main.c", want: "c"},
		"last explicit language":   {arguments: []string{"gcc", "-x", "c++", "-xc", "-c", "main.cpp"}, file: "main.cpp", want: "c"},
		"language at source":       {arguments: []string{"gcc", "-x", "c++", "-c", "main.c", "-x", "c"}, file: "main.c", want: "c++"},
		"trailing language":        {arguments: []string{"gcc", "-c", "main.c", "-x", "c++"}, file: "main.c", want: "c"},
		"same include operand":     {arguments: []string{"gcc", "-include", "main.c", "-x", "c++", "-c", "main.c"}, file: "main.c", want: "c++"},
		"trailing include operand": {arguments: []string{"gcc", "-x", "c", "-c", "main.c", "-x", "c++", "-include", "main.c"}, file: "main.c", want: "c"},
		"reset explicit language":  {arguments: []string{"gcc", "-xc++", "-x", "none", "-c", "main.c"}, file: "main.c", want: "c"},
		"Objective-C":              {arguments: []string{"clang", "-c", "main.m"}, file: "main.m", want: "objective-c"},
		"Objective-C++":            {arguments: []string{"clang++", "-c", "main.mm"}, file: "main.mm", want: "objective-c++"},
		"assembler":                {arguments: []string{"gcc", "-c", "main.s"}, file: "main.s", want: "assembler"},
		"assembler with cpp":       {arguments: []string{"gcc", "-c", "main.S"}, file: "main.S", want: "assembler-with-cpp"},
		"Clang CUDA":               {arguments: []string{"clang", "-c", "main.cu"}, file: "main.cu", want: "cuda"},
		"GCC CUDA extension":       {arguments: []string{"gcc", "-c", "main.cu"}, file: "main.cu", want: "c++"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := compilerLanguage(test.arguments, test.file); got != test.want {
				t.Fatalf("unexpected language: want %q, got %q", test.want, got)
			}
		})
	}
}

func TestCompilerFullPathUsesTrackedWorkingDirectory(t *testing.T) {
	workingDir := t.TempDir()
	compiler := filepath.Join(workingDir, "toolchain", "fake-gcc")
	if err := os.Mkdir(filepath.Dir(compiler), 0o755); err != nil {
		t.Fatalf("create compiler directory failed: %v", err)
	}
	if err := os.WriteFile(compiler, []byte("compiler"), 0o755); err != nil {
		t.Fatalf("write compiler failed: %v", err)
	}

	if got := compilerFullPath("./toolchain/fake-gcc", workingDir); got != compiler {
		t.Fatalf("unexpected compiler path: want %q, got %q", compiler, got)
	}

	t.Setenv("PATH", "toolchain")
	if got := compilerFullPath("fake-gcc", workingDir); got != compiler {
		t.Fatalf("unexpected compiler path from relative PATH: want %q, got %q", compiler, got)
	}
}

func TestExecutableCandidatesUseWindowsPathExt(t *testing.T) {
	for name, test := range map[string]struct {
		filename string
		pathExt  string
		want     []string
	}{
		"no extension": {
			filename: `C:\toolchain\gcc`,
			want:     []string{`C:\toolchain\gcc.EXE`, `C:\toolchain\gcc.CMD`},
		},
		"versioned name": {
			filename: `C:\toolchain\clang-18.1`,
			want:     []string{`C:\toolchain\clang-18.1`, `C:\toolchain\clang-18.1.EXE`, `C:\toolchain\clang-18.1.CMD`},
		},
		"executable extension": {
			filename: `C:\toolchain\gcc.exe`,
			want:     []string{`C:\toolchain\gcc.exe`},
		},
		"empty extension list": {
			filename: `C:\toolchain\gcc`,
			pathExt:  ";",
			want:     []string{`C:\toolchain\gcc`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pathExt := test.pathExt
			if pathExt == "" {
				pathExt = ".EXE;.CMD"
			}
			got := executableCandidates(test.filename, "windows", pathExt)
			if !slices.Equal(got, test.want) {
				t.Fatalf("unexpected executable candidates:\nwant: %v\ngot:  %v", test.want, got)
			}
		})
	}
}

func TestParsePredefinedMacros(t *testing.T) {
	output := []byte("#define VALUE 1\n#define EMPTY\n#define FUNC(x)\n#define TEXT \"a   b\"\n#define PATH \\\\server\\share\r\n#define __STDC__ 1\n#define __cplusplus 202002L\n#define __cpp_constexpr 202002L\nignored\n")
	want := []string{
		"-DVALUE=1",
		"-DEMPTY=",
		"-DFUNC(x)=",
		`-DTEXT="a   b"`,
		`-DPATH=\\server\share`,
	}
	if got := parsePredefinedMacros(output); !slices.Equal(got, want) {
		t.Fatalf("unexpected macros:\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestPredefinedMacroProbeArgs(t *testing.T) {
	arguments := []string{
		"gcc", "-std=c11", "--target=test", "-undef", "-fopenmp", "-DORIGINAL=1", "-UOLD",
		"-include", "config.h", "-includejoined.h", "-imacros=macros.h",
		"-MD", "-MF", "main.d", "-o", "main.o", "-c",
		"src/other.c", "src/main.c",
	}
	addArgs := []string{"-m32", "-DADDED=1"}
	configured := append(append([]string(nil), arguments...), addArgs...)
	want := []string{
		"-std=c11", "--target=test", "-undef", "-fopenmp", "-m32", "-x", "c", "-dM", "-E", "-",
	}
	if got := predefinedMacroProbeArgs(configured, "c"); !slices.Equal(got, want) {
		t.Fatalf("unexpected probe args:\nwant: %v\ngot:  %v", want, got)
	}
}

func TestPredefinedMacroProbeConsumesIgnoredOptionOperands(t *testing.T) {
	for name, arguments := range map[string][]string{
		"linker":     {"gcc", "-Xlinker", "-m32", "-m64", "-c", "main.c"},
		"output":     {"gcc", "-o", "-m32", "-m64", "-c", "main.c"},
		"dependency": {"gcc", "-MF", "-m32", "-m64", "-c", "main.c"},
	} {
		t.Run(name, func(t *testing.T) {
			want := []string{"-m64", "-x", "c", "-dM", "-E", "-"}
			probe, reason := predefinedMacroProbeArgsChecked(arguments, "c")
			if reason != "" || !slices.Equal(probe, want) {
				t.Fatalf("unexpected probe args: reason=%q\nwant: %v\ngot:  %v", reason, want, probe)
			}
		})
	}

	for _, option := range []string{"-Xclang", "-Xpreprocessor", "-mllvm", "-mmlir", "-mthread-model"} {
		arguments := []string{"clang", option, "-m32", "-c", "main.c"}
		if probe, reason := predefinedMacroProbeArgsChecked(arguments, "c"); probe != nil || reason == "" {
			t.Fatalf("forwarding option %q was not rejected: probe=%v, reason=%q", option, probe, reason)
		}
	}
	if probe, reason := predefinedMacroProbeArgsChecked([]string{"gcc", "-Wp,-undef", "-c", "main.c"}, "c"); probe != nil || reason == "" {
		t.Fatalf("preprocessor forwarding option was not rejected: probe=%v, reason=%q", probe, reason)
	}
}

func TestPredefinedMacroProbePreservesPathConfiguration(t *testing.T) {
	arguments := []string{
		"gcc", "-nostdinc", "--sysroot=/sdk", "-resource-dir", "/resource",
		"--gcc-toolchain", "/toolchain", "-c", "main.c",
	}
	want := []string{
		"-nostdinc", "--sysroot=/sdk", "-resource-dir", "/resource",
		"--gcc-toolchain", "/toolchain", "-x", "c", "-dM", "-E", "-",
	}
	probe, reason := predefinedMacroProbeArgsChecked(arguments, "c")
	if reason != "" || !slices.Equal(probe, want) {
		t.Fatalf("unexpected probe args: reason=%q\nwant: %v\ngot:  %v", reason, want, probe)
	}
}

func TestPredefinedMacroProbeHandlesCommonReleaseFlags(t *testing.T) {
	arguments := []string{
		"gcc", "-fno-omit-frame-pointer", "-ffunction-sections", "-fdata-sections",
		"-fvisibility=hidden", "-Wconversion", "-Wshadow", "-Wformat=2", "-Wno-deprecated", "-c", "main.cpp",
	}
	want := []string{"-Wno-deprecated", "-x", "c++", "-dM", "-E", "-"}
	probe, reason := predefinedMacroProbeArgsChecked(arguments, "c++")
	if reason != "" || !slices.Equal(probe, want) {
		t.Fatalf("unexpected probe args: reason=%q\nwant: %v\ngot:  %v", reason, want, probe)
	}
}

func TestProbeEnvironmentRemovesDependencyOutputs(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin",
		"DEPENDENCIES_OUTPUT=/tmp/dependencies",
		"SUNPRO_DEPENDENCIES=/tmp/sunpro",
		"KEEP=value",
	}
	want := []string{"PATH=/usr/bin", "KEEP=value"}
	if got := probeEnvironment(environment); !slices.Equal(got, want) {
		t.Fatalf("unexpected probe environment:\nwant: %v\ngot:  %v", want, got)
	}
	wantWithPWD := []string{"PATH=/usr/bin", "KEEP=value", "PWD=/project"}
	if got := probeEnvironment(append(environment, "PWD=/caller"), "/project"); !slices.Equal(got, wantWithPWD) {
		t.Fatalf("unexpected probe PWD environment:\nwant: %v\ngot:  %v", wantWithPWD, got)
	}
}

func TestUnsafeCompilerEnvironment(t *testing.T) {
	for _, name := range []string{
		"CCC_OVERRIDE_OPTIONS", "CCC_PRINT_OPTIONS_FILE", "GCC_EXEC_PREFIX", "COMPILER_PATH",
		"LD_PRELOAD", "LD_AUDIT", "DYLD_INSERT_LIBRARIES",
	} {
		if got := unsafeCompilerEnvironment([]string{name + "=value"}); got != name {
			t.Fatalf("compiler-control environment %s was not rejected: %q", name, got)
		}
	}
	if got := unsafeCompilerEnvironment([]string{"PATH=/usr/bin", "KEEP=value"}); got != "" {
		t.Fatalf("safe environment was rejected: %q", got)
	}
}

func TestCompilerControlEnvironmentDoesNotExecuteProbe(t *testing.T) {
	for name, value := range map[string]string{
		"CCC_OVERRIDE_OPTIONS": "+-Xclang +-load +-Xclang +/tmp/evil.so",
		"LD_PRELOAD":           "/tmp/evil.so",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			tool := newTestTool(t, Config{})
			calls := 0
			tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
				calls++
				return exec.Command(os.Args[0])
			}
			if macros := tool.getPredefinedMacros([]string{"clang", "-c", "main.c"}, "main.c", t.TempDir()); macros != nil {
				t.Fatalf("expected compiler-control environment to skip probing, got %v", macros)
			}
			if calls != 0 {
				t.Fatalf("unsafe environment executed the compiler %d times", calls)
			}
		})
	}
}

func TestResolvedCompilerWrapperSkipsPredefinedMacros(t *testing.T) {
	for _, wrapper := range []string{"ccache", "icecc"} {
		t.Run(wrapper, func(t *testing.T) {
			workingDir := t.TempDir()
			wrapperPath := filepath.Join(workingDir, wrapper)
			if err := os.WriteFile(wrapperPath, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
				t.Fatalf("write wrapper failed: %v", err)
			}
			compiler := filepath.Join(workingDir, "gcc")
			if err := os.Symlink(wrapperPath, compiler); err != nil {
				t.Fatalf("create compiler symlink failed: %v", err)
			}

			tool := newTestTool(t, Config{})
			calls := 0
			tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
				calls++
				return exec.Command(os.Args[0])
			}
			if macros := tool.getPredefinedMacros([]string{compiler, "-c", "main.c"}, "main.c", workingDir); macros != nil {
				t.Fatalf("expected resolved %s wrapper to skip probing, got %v", wrapper, macros)
			}
			if calls != 0 {
				t.Fatalf("resolved %s wrapper executed compiler %d times", wrapper, calls)
			}
		})
	}
}

func TestPredefinedMacroProbeDoesNotInheritDependencyOutputs(t *testing.T) {
	workingDir := t.TempDir()
	compiler := filepath.Join(workingDir, "fake-gcc")
	script := `#!/bin/sh
test -n "$DEPENDENCIES_OUTPUT" && printf dependency > "$DEPENDENCIES_OUTPUT"
test -n "$SUNPRO_DEPENDENCIES" && printf dependency > "$SUNPRO_DEPENDENCIES"
printf '%s\n' '#define SAFE_PROBE 1'
`
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatalf("write compiler failed: %v", err)
	}
	dependencies := filepath.Join(workingDir, "dependencies.out")
	sunpro := filepath.Join(workingDir, "sunpro.out")
	t.Setenv("DEPENDENCIES_OUTPUT", dependencies)
	t.Setenv("SUNPRO_DEPENDENCIES", sunpro)

	tool := newTestTool(t, Config{})
	macros := tool.getPredefinedMacros([]string{compiler, "-c", "main.c"}, "main.c", workingDir)
	if !slices.Contains(macros, "-DSAFE_PROBE=1") {
		t.Fatalf("expected probe macros, got %v", macros)
	}
	for _, filename := range []string{dependencies, sunpro} {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			t.Fatalf("probe created dependency output %q: %v", filename, err)
		}
	}
}

func TestPredefinedMacroProbeRejectsCompilerComponentOptions(t *testing.T) {
	for _, arguments := range [][]string{
		{"gcc", "-B", "/toolchain", "-c", "main.c"},
		{"gcc", "-B/toolchain", "-c", "main.c"},
		{"gcc", "-specs", "custom.specs", "-c", "main.c"},
		{"gcc", "-wrapper", "helper", "-c", "main.c"},
		{"clang", "-ivfsoverlay", "overlay.yaml", "-c", "main.c"},
		{"clang", "--config", "unsafe.cfg", "-c", "main.c"},
	} {
		if probe, reason := predefinedMacroProbeArgsChecked(arguments, "c"); probe != nil || reason == "" {
			t.Fatalf("component option was not rejected: arguments=%v probe=%v reason=%q", arguments, probe, reason)
		}
	}
}

func TestUnsafeMachineOptionDoesNotExecuteProbe(t *testing.T) {
	tool := newTestTool(t, Config{})
	calls := 0
	tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
		calls++
		return exec.Command(os.Args[0])
	}

	arguments := []string{"clang", "-mmlir", "ignored", "-x", "c", "-fplugin=/tmp/evil.so", "-c", "main.c"}
	if macros := tool.getPredefinedMacros(arguments, "main.c", t.TempDir()); macros != nil {
		t.Fatalf("expected no macros for an unsafe option, got %v", macros)
	}
	if calls != 0 {
		t.Fatalf("unsafe language must not execute the compiler, got %d calls", calls)
	}
	if isSafePredefinedMacroProbeOption("-mmlir") || isSafePredefinedMacroProbeOption("-mthread-model") {
		t.Fatal("compiler forwarding options must not be treated as safe machine options")
	}
}

func TestUnsafePluginOptionDoesNotExecuteProbe(t *testing.T) {
	tool := newTestTool(t, Config{})
	calls := 0
	tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
		calls++
		return exec.Command(os.Args[0])
	}

	if macros := tool.getPredefinedMacros([]string{"gcc", "-fplugin=/tmp/evil.so", "-c", "main.c"}, "main.c", t.TempDir()); macros != nil {
		t.Fatalf("expected no macros for a plugin option, got %v", macros)
	}
	if calls != 0 {
		t.Fatalf("plugin option must not execute the compiler, got %d calls", calls)
	}
}

func TestRealCompilerMacroAffectingOptionsAreReplayed(t *testing.T) {
	compiler, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc is not available")
	}
	tool := newTestTool(t, Config{})
	workingDir := t.TempDir()

	native := tool.getPredefinedMacros([]string{compiler, "-march=native", "-c", "main.c"}, "main.c", workingDir)
	if slices.Contains(native, "-D__AES__=1") {
		withoutAES := tool.getPredefinedMacros([]string{compiler, "-march=native", "-mno-aes", "-c", "main.c"}, "main.c", workingDir)
		if slices.Contains(withoutAES, "-D__AES__=1") {
			t.Fatal("-mno-aes probe retained the stale __AES__ macro")
		}
	}

	withoutPIC := tool.getPredefinedMacros([]string{compiler, "-fPIC", "-fno-pic", "-c", "main.c"}, "main.c", workingDir)
	for _, macro := range withoutPIC {
		if macro == "-D__PIC__=1" || macro == "-D__PIC__=2" || macro == "-D__PIE__=1" || macro == "-D__PIE__=2" {
			t.Fatalf("-fno-pic probe retained a stale position-independent macro: %s", macro)
		}
	}

	longDouble64 := tool.getPredefinedMacros([]string{compiler, "-mlong-double-64", "-c", "main.c"}, "main.c", workingDir)
	if !slices.Contains(longDouble64, "-D__SIZEOF_LONG_DOUBLE__=8") {
		t.Fatalf("expected 64-bit long double macros, got %v", longDouble64)
	}
}

func TestClangCUDAMacroProbeIsSkipped(t *testing.T) {
	compiler, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang is not available")
	}
	tool := newTestTool(t, Config{})
	macros := tool.getPredefinedMacros(
		[]string{compiler, "-nocudainc", "-nocudalib", "-c", "main.cu"},
		"main.cu",
		t.TempDir(),
	)
	if macros != nil {
		t.Fatalf("expected multi-stage CUDA macro probe to be skipped, got %v", macros)
	}
}

func TestPredefinedMacroResponseFileIsNotExecuted(t *testing.T) {
	workingDir := t.TempDir()
	responseFile := filepath.Join(workingDir, "arguments.rsp")
	victim := filepath.Join(workingDir, "victim")
	if err := os.WriteFile(responseFile, []byte("-o "+victim), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatalf("write victim failed: %v", err)
	}

	tool := newTestTool(t, Config{})
	calls := 0
	tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
		calls++
		return exec.Command(os.Args[0])
	}
	arguments := []string{"gcc", "@arguments.rsp", "-c", "main.c"}
	if macros := tool.getPredefinedMacros(arguments, "main.c", workingDir); macros != nil {
		t.Fatalf("expected no macros for a response file, got %v", macros)
	}
	if macros := tool.getPredefinedMacros(arguments, "main.c", workingDir); macros != nil {
		t.Fatalf("expected cached response-file failure, got %v", macros)
	}
	if calls != 0 {
		t.Fatalf("response file must not execute the compiler, got %d calls", calls)
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim failed: %v", err)
	}
	if string(data) != "original" {
		t.Fatalf("response file changed victim content: %q", data)
	}
}

func TestParseAddsAndCachesPredefinedMacrosByConfiguration(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       true,
		AddArgs:      []string{"-DUSER=1"},
	})

	var calls [][]string
	tool.compilerCommand = func(_ string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string(nil), args...))
		cmd := exec.Command(os.Args[0], "-test.run=^TestCompilerMacrosHelperProcess$")
		cmd.Env = append(os.Environ(), "COMPILEDB_TEST_COMPILER_HELPER=success")
		return cmd
	}

	tool.Parse([]string{
		"fake-gcc -std=c11 -DORIGINAL=1 -UEMPTY -c one.c",
		"fake-gcc -std=c11 -DORIGINAL=1 -UEMPTY -c two.c",
		"fake-gcc -std=c17 -DORIGINAL=1 -UEMPTY -c three.c",
	})

	if len(calls) != 2 {
		t.Fatalf("expected one compiler call per configuration, got %d: %v", len(calls), calls)
	}
	wantCalls := [][]string{
		{"-std=c11", "-x", "c", "-dM", "-E", "-"},
		{"-std=c17", "-x", "c", "-dM", "-E", "-"},
	}
	for i := range wantCalls {
		if !slices.Equal(calls[i], wantCalls[i]) {
			t.Fatalf("unexpected compiler calls:\nwant: %v\ngot:  %v", wantCalls, calls)
		}
	}

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 3 {
		t.Fatalf("expected 3 commands, got %#v", commands)
	}
	standards := []string{"-std=c11", "-std=c11", "-std=c17"}
	for i, command := range commands {
		assertArgumentOrder(t, command.Arguments,
			"fake-gcc", "-DORIGINAL=0", "-DEMPTY=", standards[i], "-DORIGINAL=1", "-UEMPTY", "-DUSER=1",
		)
		macroIndex := slices.Index(command.Arguments, "-DORIGINAL=0")
		originalIndex := slices.Index(command.Arguments, "-DORIGINAL=1")
		undefIndex := slices.Index(command.Arguments, "-UEMPTY")
		addIndex := slices.Index(command.Arguments, "-DUSER=1")
		if macroIndex < 0 || originalIndex <= macroIndex || undefIndex <= macroIndex || addIndex <= originalIndex {
			t.Fatalf("unexpected macro precedence in arguments: %v", command.Arguments)
		}
	}
}

func TestParseExpandsResponseFileBeforeMacroProbe(t *testing.T) {
	workingDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("-std=c11 -DORIGINAL=1 -c main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       true,
	})
	var compilerArguments []string
	tool.compilerCommand = func(_ string, arguments ...string) *exec.Cmd {
		compilerArguments = append([]string(nil), arguments...)
		cmd := exec.Command(os.Args[0], "-test.run=^TestCompilerMacrosHelperProcess$")
		cmd.Env = append(os.Environ(), "COMPILEDB_TEST_COMPILER_HELPER=success")
		return cmd
	}

	tool.Parse([]string{"fake-gcc @arguments.rsp"})

	wantProbe := []string{"-std=c11", "-x", "c", "-dM", "-E", "-"}
	if !slices.Equal(compilerArguments, wantProbe) {
		t.Fatalf("macro probe received unexpanded response arguments:\nwant: %v\ngot:  %v", wantProbe, compilerArguments)
	}
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, "-DORIGINAL=0") ||
		slices.Contains(commands[0].Arguments, "@arguments.rsp") {
		t.Fatalf("response-file macro command was not flattened: %#v", commands)
	}
}

func TestPredefinedMacroFailureKeepsCommandsAndIsCached(t *testing.T) {
	for _, mode := range []string{"missing", "exit"} {
		t.Run(mode, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				Macros:       true,
			})
			var logs bytes.Buffer
			tool.Logger.SetOutput(&logs)

			calls := 0
			tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
				calls++
				if mode == "missing" {
					return exec.Command(filepath.Join(t.TempDir(), "missing-compiler"))
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestCompilerMacrosHelperProcess$")
				cmd.Env = append(os.Environ(), "COMPILEDB_TEST_COMPILER_HELPER=exit")
				return cmd
			}

			tool.Parse([]string{"fake-gcc -c one.c", "fake-gcc -c two.c"})

			if calls != 1 {
				t.Fatalf("expected failed result to be cached, got %d compiler calls", calls)
			}
			if !bytes.Contains(logs.Bytes(), []byte("failed to get predefined macros from fake-gcc")) {
				t.Fatalf("expected compiler failure log, got %q", logs.String())
			}
			if commands := readCompilerTestCommands(t, outputFile); len(commands) != 2 {
				t.Fatalf("expected compiler commands to be preserved, got %#v", commands)
			}
		})
	}
}

func TestParseSkipsMacroProbeForCompilerOverrides(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) string{
		"ccache option": func(_ *testing.T) string {
			return "ccache compiler=clang gcc -c main.c"
		},
		"ccache environment": func(t *testing.T) string {
			t.Setenv("CCACHE_COMPILER", "clang")
			return "ccache gcc -c main.c"
		},
		"shell environment": func(_ *testing.T) string {
			return "PATH=/toolchain fake-gcc -c main.c"
		},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				Macros:       true,
				FullPath:     true,
			})
			calls := 0
			tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
				calls++
				return exec.Command(os.Args[0])
			}

			tool.Parse([]string{setup(t)})
			commands := readCompilerTestCommands(t, outputFile)
			if calls != 0 || len(commands) != 1 {
				t.Fatalf("unsafe compiler override was probed: calls=%d commands=%#v", calls, commands)
			}
			if name == "shell environment" && commands[0].Arguments[0] != "fake-gcc" {
				t.Fatalf("leading assignment remained in compiler argv: %v", commands[0].Arguments)
			}
		})
	}
}

func TestPredefinedMacroCacheTracksCompilerReplacement(t *testing.T) {
	workingDir := t.TempDir()
	compiler := filepath.Join(workingDir, "mutable-gcc")
	writeCompiler := func(macro string, timestamp int64) {
		t.Helper()
		contents := "#!/bin/sh\necho '#define " + macro + " 1'\n"
		if err := os.WriteFile(compiler, []byte(contents), 0o755); err != nil {
			t.Fatalf("write compiler failed: %v", err)
		}
		when := time.Unix(timestamp, 0)
		if err := os.Chtimes(compiler, when, when); err != nil {
			t.Fatalf("set compiler timestamp failed: %v", err)
		}
	}

	tool := newTestTool(t, Config{})
	writeCompiler("CACHE_A", 1)
	first := tool.getPredefinedMacros([]string{compiler, "-c", "one.c"}, "one.c", workingDir)
	writeCompiler("CACHE_B", 2)
	second := tool.getPredefinedMacros([]string{compiler, "-c", "two.c"}, "two.c", workingDir)
	if !slices.Contains(first, "-DCACHE_A=1") || !slices.Contains(second, "-DCACHE_B=1") {
		t.Fatalf("compiler replacement reused stale macros: first=%v second=%v", first, second)
	}
}

func TestPredefinedMacroProbeReturnsWhenCanceled(t *testing.T) {
	workingDir := t.TempDir()
	startedFile := filepath.Join(workingDir, "probe.started")
	compiler := filepath.Join(workingDir, "slow-gcc")
	script := "#!/bin/sh\nprintf started > " + startedFile + "\nexec sleep 30\n"
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatalf("write compiler failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	tool := newTestTool(t, Config{})
	tool.Context = ctx
	done := make(chan []string, 1)
	go func() {
		done <- tool.getPredefinedMacros([]string{compiler, "-c", "main.c"}, "main.c", workingDir)
	}()

	waitForTestFile(t, startedFile)
	cancel()
	select {
	case macros := <-done:
		if macros != nil {
			t.Fatalf("canceled probe returned macros: %v", macros)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("predefined macro probe did not return after cancellation")
	}
}

func TestRealCompilerPredefinedMacrosReplayWithWerror(t *testing.T) {
	for _, compilerName := range []string{"gcc", "clang"} {
		t.Run(compilerName, func(t *testing.T) {
			compiler, err := exec.LookPath(compilerName)
			if err != nil {
				t.Skipf("%s is not available", compilerName)
			}
			sources := []string{"main.c", "main.cpp"}
			if compilerName == "clang" {
				sources = append(sources, "main.m", "main.mm")
			}
			for _, source := range sources {
				t.Run(filepath.Ext(source), func(t *testing.T) {
					workingDir := t.TempDir()
					if err := os.WriteFile(filepath.Join(workingDir, source), []byte("int value;\n"), 0o644); err != nil {
						t.Fatalf("write source failed: %v", err)
					}
					outputFile := filepath.Join(workingDir, "compile_commands.json")
					tool := newTestTool(t, Config{
						InputFile:    "stdin",
						OutputFile:   outputFile,
						BuildDir:     workingDir,
						RegexCompile: RegexCompile,
						RegexFile:    RegexFile,
						Macros:       true,
					})
					tool.Parse([]string{compiler + " -Werror -c " + source})

					commands := readCompilerTestCommands(t, outputFile)
					if len(commands) != 1 {
						t.Fatalf("expected one command, got %#v", commands)
					}
					command := exec.Command(commands[0].Arguments[0], commands[0].Arguments[1:]...)
					command.Dir = commands[0].Directory
					if output, err := command.CombinedOutput(); err != nil {
						t.Fatalf("generated command failed: %v\n%s", err, output)
					}
				})
			}
		})
	}
}

func TestCCacheCommandSkipsPredefinedMacros(t *testing.T) {
	projectDir := t.TempDir()
	compilerDir := filepath.Join(projectDir, "toolchain")
	if err := os.Mkdir(compilerDir, 0o755); err != nil {
		t.Fatalf("create compiler directory failed: %v", err)
	}
	compiler := filepath.Join(compilerDir, "fake-gcc")
	script := "#!/bin/sh\nprintf '%s\\n' '#define FROM_RELATIVE_COMPILER 1'\n"
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake compiler failed: %v", err)
	}

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       true,
		FullPath:     true,
	})
	tool.Parse([]string{"ccache compiler_check=content ./toolchain/fake-gcc -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || slices.Contains(commands[0].Arguments, "-DFROM_RELATIVE_COMPILER=1") {
		t.Fatalf("ccache command did not skip macro probing: %#v", commands)
	}
	if len(commands[0].Arguments) < 3 || commands[0].Arguments[2] != hostPathToDatabasePath(compiler) {
		t.Fatalf("expected full path for wrapped compiler, got %#v", commands[0].Arguments)
	}
}

func TestCompilerMacrosHelperProcess(t *testing.T) {
	switch os.Getenv("COMPILEDB_TEST_COMPILER_HELPER") {
	case "success":
		fmt.Println("#define ORIGINAL 0")
		fmt.Println("#define EMPTY")
	case "exit":
		fmt.Println("#define PARTIAL 1")
		fmt.Fprintln(os.Stderr, "probe failed")
		os.Exit(2)
	default:
		return
	}
	os.Exit(0)
}

func readCompilerTestCommands(t *testing.T, filename string) []Command {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("decode output failed: %v", err)
	}
	return commands
}

func assertArgumentOrder(t *testing.T, arguments []string, values ...string) {
	t.Helper()
	last := -1
	for _, value := range values {
		index := slices.Index(arguments, value)
		if index <= last {
			t.Fatalf("expected %q after index %d in %v", value, last, arguments)
		}
		last = index
	}
}

func waitForTestFile(t *testing.T, filename string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filename); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filename)
}
