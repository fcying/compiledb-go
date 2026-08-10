package internal

import (
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var compilerWrappers = map[string]struct{}{
	"ccache":  {},
	"distcc":  {},
	"icecc":   {},
	"sccache": {},
}

type compilerInvocation struct {
	compilerIndex int
	optionsStart  int
	compiler      string
	explicit      bool
	valid         bool
	probeDisabled string
}

type compilerArgumentScan struct {
	language        string
	terminatorIndex int
	inputIndexes    []int
	inputLanguages  []string
}

func executableBase(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = strings.ToLower(path.Base(name))
	return strings.TrimSuffix(name, ".exe")
}

func parseCompilerInvocation(arguments []string) compilerInvocation {
	index := 0
	lastWrapper := ""
	probeDisabled := ""
	for index < len(arguments) {
		wrapper := executableBase(arguments[index])
		if _, ok := compilerWrappers[wrapper]; !ok {
			break
		}
		lastWrapper = wrapper
		index++
		if wrapper == "ccache" {
			probeDisabled = "ccache compiler selection cannot be determined safely"
			for index < len(arguments) && isCCacheConfigAssignment(arguments[index]) {
				index++
			}
		} else if wrapper == "icecc" {
			probeDisabled = "icecc compiler selection cannot be determined safely"
		}
	}
	if index >= len(arguments) {
		return compilerInvocation{}
	}
	if (lastWrapper == "distcc" || lastWrapper == "icecc") && strings.HasPrefix(arguments[index], "-") {
		return compilerInvocation{
			compilerIndex: index,
			optionsStart:  index,
			compiler:      "cc",
			valid:         true,
			probeDisabled: probeDisabled,
		}
	}
	if lastWrapper != "" && strings.HasPrefix(arguments[index], "-") {
		return compilerInvocation{}
	}
	invocation := compilerInvocation{
		compilerIndex: index,
		optionsStart:  index + 1,
		compiler:      arguments[index],
		explicit:      true,
		valid:         true,
	}
	invocation.probeDisabled = probeDisabled
	return invocation
}

func compilerArgumentIndex(arguments []string) int {
	return parseCompilerInvocation(arguments).compilerIndex
}

func isCCacheConfigAssignment(argument string) bool {
	name, _, ok := strings.Cut(argument, "=")
	return ok && name != "" && !strings.HasPrefix(name, "-") && !strings.ContainsAny(name, `/\`)
}

func isShellAssignment(argument string) bool {
	name, _, ok := strings.Cut(argument, "=")
	if !ok || name == "" {
		return false
	}
	for i, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' ||
			(i > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func compilerFullPath(compiler, workingDir string) string {
	baseDir := filepath.FromSlash(workingDir)
	if !filepath.IsAbs(baseDir) {
		absolute, err := filepath.Abs(baseDir)
		if err != nil {
			return ""
		}
		baseDir = absolute
	}

	candidate := filepath.FromSlash(compiler)
	if strings.ContainsAny(compiler, `/\`) {
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(baseDir, candidate)
		}
		return findExecutable(candidate)
	}

	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			if runtime.GOOS == "windows" {
				continue
			}
			directory = baseDir
		} else if !filepath.IsAbs(directory) {
			directory = filepath.Join(baseDir, directory)
		}
		if fullPath := findExecutable(filepath.Join(directory, compiler)); fullPath != "" {
			return fullPath
		}
	}
	return ""
}

func executableCandidates(filename, goos, pathExt string) []string {
	if goos != "windows" {
		return []string{filename}
	}
	extensions := windowsExecutableExtensions(pathExt)
	if len(extensions) == 0 {
		return []string{filename}
	}
	for _, extension := range extensions {
		if strings.EqualFold(filepath.Ext(filename), extension) {
			return []string{filename}
		}
	}
	candidates := make([]string, 0, len(extensions)+1)
	if filepath.Ext(filename) != "" {
		candidates = append(candidates, filename)
	}
	for _, extension := range extensions {
		candidates = append(candidates, filename+extension)
	}
	return candidates
}

func windowsExecutableExtensions(pathExt string) []string {
	if pathExt == "" {
		pathExt = ".COM;.EXE;.BAT;.CMD"
	}
	extensions := make([]string, 0, 4)
	for _, extension := range strings.Split(pathExt, ";") {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			continue
		}
		if extension[0] != '.' {
			extension = "." + extension
		}
		extensions = append(extensions, extension)
	}
	return extensions
}

func findExecutable(filename string) string {
	for _, candidate := range executableCandidates(filename, runtime.GOOS, os.Getenv("PATHEXT")) {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
			continue
		}
		fullPath, err := filepath.Abs(candidate)
		if err == nil {
			return fullPath
		}
	}
	return ""
}

func compilerCacheIdentity(filename string) string {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		absolute = filename
	}
	resolved, err := filepath.EvalSymlinks(filename)
	if err != nil {
		resolved = filename
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return absolute + "\x00" + resolved
	}
	return absolute + "\x00" + resolved + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

func compilerResolvedWrapper(filename string) string {
	resolved, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return ""
	}
	switch wrapper := executableBase(resolved); wrapper {
	case "ccache", "icecc":
		return wrapper
	default:
		return ""
	}
}

func inferredCompilerLanguage(compiler, sourceFile string) string {
	extension := path.Ext(sourceFile)
	switch extension {
	case ".C":
		return "c++"
	case ".S":
		return "assembler-with-cpp"
	}
	switch strings.ToLower(extension) {
	case ".cc", ".cpp", ".cxx", ".c++":
		return "c++"
	case ".cu":
		if strings.Contains(executableBase(compiler), "clang") {
			return "cuda"
		}
		return "c++"
	case ".m":
		return "objective-c"
	case ".mm":
		return "objective-c++"
	case ".s":
		return "assembler"
	}

	if strings.Contains(executableBase(compiler), "++") {
		return "c++"
	}
	return "c"
}

func compilerOptionTakesArgument(argument string) bool {
	switch argument {
	case "-o", "--output", "-MF", "-MT", "-MQ", "-MJ",
		"-include", "--include", "-imacros", "--imacros", "-include-pch",
		"-dependency-file", "--dependency-file", "-T", "--script", "-idirafter", "-iprefix",
		"-ivfsoverlay", "-vfsoverlay", "-working-directory", "-serialize-diagnostics",
		"-iwithprefix", "-iwithprefixbefore", "-iwithsysroot", "-isystem", "-isystem-after", "-iquote",
		"-stdlib++-isystem", "--config", "-imultilib", "-imultiarch", "-index-store-path", "-fdebug-compilation-dir",
		"-F", "-iframework", "-iframeworkwithsysroot",
		"-I", "-D", "-U", "-L", "-l", "-B", "-x", "-A", "-z", "-u", "-iapinotes-modules",
		"-Xlinker", "-Xclang", "-Xassembler", "-Xpreprocessor", "-mllvm", "-mmlir", "-mthread-model",
		"-fplugin", "-arch", "-march", "-mcpu", "-mtune", "-mabi", "-mfpu",
		"-mfloat-abi", "-target", "--target", "--sysroot", "-isysroot",
		"--gcc-toolchain", "-gcc-toolchain", "-resource-dir", "--cuda-path",
		"--cuda-gpu-arch", "--offload-arch", "-Xcuda-fatbinary", "-Xcuda-ptxas",
		"--param", "-specs", "-wrapper", "-aux-info", "-dumpbase", "-dumpbase-ext", "-dumpdir":
		return true
	default:
		return strings.HasPrefix(argument, "-X") && !strings.Contains(argument, "=")
	}
}

func scanCompilerArguments(arguments []string, optionsStart int, compiler, sourceFile string) compilerArgumentScan {
	scan := compilerArgumentScan{terminatorIndex: -1}
	explicitLanguage := ""
	sourceLanguage := ""
	options := true

	for i := optionsStart; i < len(arguments); i++ {
		argument := arguments[i]
		if options && argument == "--" {
			scan.terminatorIndex = i
			options = false
			continue
		}
		if options && compilerOptionTakesArgument(argument) {
			if i+1 >= len(arguments) {
				continue
			}
			if argument == "-x" {
				explicitLanguage = arguments[i+1]
			}
			i++
			continue
		}
		if options && strings.HasPrefix(argument, "-x") && len(argument) > 2 {
			explicitLanguage = argument[2:]
			continue
		}
		if options && strings.HasPrefix(argument, "-") {
			continue
		}
		scan.inputIndexes = append(scan.inputIndexes, i)
		scan.inputLanguages = append(scan.inputLanguages, explicitLanguage)
		if isSourceArgument(argument, sourceFile) {
			if explicitLanguage != "" && explicitLanguage != "none" {
				sourceLanguage = explicitLanguage
			} else {
				sourceLanguage = inferredCompilerLanguage(compiler, sourceFile)
			}
		}
	}

	if sourceLanguage == "" {
		sourceLanguage = inferredCompilerLanguage(compiler, sourceFile)
	}
	scan.language = sourceLanguage
	return scan
}

func sourceFilesFromArguments(arguments []string, invocation compilerInvocation) []string {
	scan := scanCompilerArguments(arguments, invocation.optionsStart, invocation.compiler, "")
	files := make([]string, 0, len(scan.inputIndexes))
	for i, index := range scan.inputIndexes {
		explicitLanguage := scan.inputLanguages[i]
		if strings.HasPrefix(arguments[index], "@") {
			continue
		}
		if hasSourceExtension(arguments[index]) || explicitLanguage != "" && explicitLanguage != "none" {
			files = append(files, arguments[index])
		}
	}
	return files
}

func pathWithoutSeparators(value string) string {
	return strings.ReplaceAll(ConvertPath(value), "/", "")
}

func hasSourceExtension(argument string) bool {
	extension := path.Ext(argument)
	if extension == ".C" || extension == ".S" {
		return true
	}
	switch strings.ToLower(extension) {
	case ".c", ".cpp", ".cc", ".cxx", ".c++", ".s", ".m", ".mm", ".cu":
		return true
	default:
		return false
	}
}

func compilerLanguage(arguments []string, sourceFile string) string {
	if len(arguments) == 0 {
		return inferredCompilerLanguage("", sourceFile)
	}
	return scanCompilerArguments(arguments, 1, arguments[0], sourceFile).language
}

func supportedCompilerLanguage(language string) bool {
	switch language {
	case "c", "c-header", "cpp-output",
		"c++", "c++-header", "c++-cpp-output",
		"objective-c", "objective-c-header", "objective-c-cpp-output",
		"objective-c++", "objective-c++-header", "objective-c++-cpp-output",
		"assembler", "assembler-with-cpp", "cuda", "cuda-cpp-output", "hip":
		return true
	default:
		return false
	}
}

func isSinglePhaseCompilerLanguage(language string) bool {
	return language != "cuda" && language != "cuda-cpp-output" && language != "hip"
}

func parsePredefinedMacros(output []byte) []string {
	macros := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSuffix(line, "\r")
		definition, ok := strings.CutPrefix(line, "#define ")
		if !ok || definition == "" {
			continue
		}

		name := definition
		value := ""
		if separator := strings.IndexAny(definition, " \t"); separator >= 0 {
			name = definition[:separator]
			value = strings.TrimLeft(definition[separator:], " \t")
		}
		if isBuiltinMacroWithReplayDiagnostic(name) {
			continue
		}
		macros = append(macros, "-D"+name+"="+value)
	}
	return macros
}

func isBuiltinMacroWithReplayDiagnostic(name string) bool {
	return name == "__cplusplus" ||
		strings.HasPrefix(name, "__cpp") ||
		strings.HasPrefix(name, "__STDC") ||
		strings.HasPrefix(name, "__STDCPP")
}

func isSourceArgument(argument, sourceFile string) bool {
	return ConvertPath(argument) == ConvertPath(sourceFile)
}

func predefinedMacroProbeArgsChecked(arguments []string, language string) ([]string, string) {
	probe := make([]string, 0, len(arguments)+5)
	options := true

	// Replay only options known to affect compiler built-ins without reading inputs or writing outputs.
	for i := 1; i < len(arguments); i++ {
		argument := arguments[i]
		if options && argument == "--" {
			options = false
			continue
		}
		if !options {
			continue
		}
		if compilerOptionTakesArgument(argument) {
			if i+1 >= len(arguments) {
				return nil, "missing operand for " + argument
			}
			operand := arguments[i+1]
			if isSafePredefinedMacroProbeOptionWithArgument(argument) {
				probe = append(probe, argument, operand)
			} else if isUnsafePredefinedMacroProbeOption(argument) {
				return nil, "unsupported option " + argument
			} else if !isIgnoredPredefinedMacroProbeOptionWithArgument(argument) {
				return nil, "unsupported option " + argument
			}
			i++
			continue
		}
		if isSafePredefinedMacroProbeOption(argument) {
			probe = append(probe, argument)
		} else if strings.HasPrefix(argument, "-") {
			if isIgnoredPredefinedMacroProbeOption(argument) {
				continue
			}
			return nil, "unsupported option " + argument
		}
	}

	return append(probe, "-x", language, "-dM", "-E", "-"), ""
}

func isIgnoredPredefinedMacroProbeOption(argument string) bool {
	if strings.HasPrefix(argument, "-Wp,") {
		return false
	}
	switch argument {
	case "-c", "-S", "-E", "-M", "-MM", "-MD", "-MMD", "-MG", "-MP",
		"-pipe", "-v", "--verbose", "-###",
		"-no-canonical-prefixes", "-Qunused-arguments",
		"-fomit-frame-pointer", "-fno-omit-frame-pointer", "-ffunction-sections", "-fdata-sections",
		"-w", "-Wall", "-Wextra", "-Werror", "-Wpedantic", "-pedantic", "-pedantic-errors":
		return true
	}
	if strings.HasPrefix(argument, "-Werror=") || strings.HasPrefix(argument, "-Wno-error=") {
		return true
	}
	if strings.HasPrefix(argument, "-W") {
		return true
	}
	for _, prefix := range []string{
		"-D", "-U", "-I", "-L", "-l", "-o", "-MF", "-MT", "-MQ", "-MJ",
		"-include", "--include", "-imacros", "--imacros", "-dependency-file", "--dependency-file",
		"-T", "--script", "-serialize-diagnostics", "-idirafter", "-iprefix", "-iwithprefix", "-isystem", "-iquote",
		"-g", "-save-temps", "-rtlib=", "-unwindlib=", "-fvisibility=",
	} {
		if strings.HasPrefix(argument, prefix) {
			return true
		}
	}
	return false
}

func isIgnoredPredefinedMacroProbeOptionWithArgument(argument string) bool {
	switch argument {
	case "-o", "--output", "-MF", "-MT", "-MQ", "-MJ",
		"-include", "--include", "-imacros", "--imacros", "-include-pch",
		"-dependency-file", "--dependency-file", "-T", "--script", "-idirafter", "-iprefix",
		"-serialize-diagnostics",
		"-iwithprefix", "-iwithprefixbefore", "-iwithsysroot", "-isystem", "-isystem-after", "-iquote",
		"-stdlib++-isystem", "--config", "-imultilib", "-imultiarch", "-index-store-path", "-fdebug-compilation-dir",
		"-F", "-iframework", "-iframeworkwithsysroot",
		"-I", "-D", "-U", "-L", "-l", "-x", "-A", "-z", "-u", "-iapinotes-modules", "-Xlinker", "-Xassembler":
		return true
	default:
		return false
	}
}

func predefinedMacroProbeArgs(arguments []string, language string) []string {
	probe, _ := predefinedMacroProbeArgsChecked(arguments, language)
	return probe
}

func isSafePredefinedMacroProbeOptionWithArgument(argument string) bool {
	switch argument {
	case "-arch", "-march", "-mcpu", "-mtune", "-mabi", "-mfpu", "-mfloat-abi",
		"-target", "--target", "--sysroot", "-isysroot", "--gcc-toolchain", "-gcc-toolchain",
		"-resource-dir", "-working-directory", "--cuda-path":
		return true
	default:
		return false
	}
}

func isSafePredefinedMacroProbeOption(argument string) bool {
	if strings.HasPrefix(argument, "-std=") ||
		strings.HasPrefix(argument, "--std=") ||
		strings.HasPrefix(argument, "-target=") ||
		strings.HasPrefix(argument, "--target=") ||
		strings.HasPrefix(argument, "--sysroot=") ||
		strings.HasPrefix(argument, "-isysroot=") ||
		strings.HasPrefix(argument, "--gcc-toolchain=") ||
		strings.HasPrefix(argument, "-gcc-toolchain=") ||
		strings.HasPrefix(argument, "-resource-dir=") ||
		strings.HasPrefix(argument, "-working-directory=") ||
		strings.HasPrefix(argument, "-stdlib=") ||
		strings.HasPrefix(argument, "--cuda-path=") {
		return true
	}
	if isSafeMachineOption(argument) {
		return true
	}
	switch argument {
	case "-O", "-O0", "-O1", "-O2", "-O3", "-Og", "-Os", "-Oz", "-Ofast":
		return true
	}
	switch argument {
	case "-ansi", "-pthread", "-undef", "-nostdinc", "-nostdinc++",
		"-Wdeprecated", "-Wno-deprecated",
		"-fPIC", "-fpic", "-fPIE", "-fpie",
		"-fno-PIC", "-fno-pic", "-fno-PIE", "-fno-pie",
		"-fexceptions", "-fno-exceptions", "-frtti", "-fno-rtti",
		"-ffreestanding", "-fhosted", "-fms-extensions", "-fms-compatibility",
		"-fshort-wchar", "-fsigned-char", "-funsigned-char", "-fopenmp", "-fno-openmp",
		"-fopenacc", "-fno-openacc", "-ffast-math", "-fno-fast-math",
		"-nocudainc", "-nocudalib":
		return true
	default:
		return strings.HasPrefix(argument, "-fopenmp=")
	}
}

func isUnsafePredefinedMacroProbeOption(argument string) bool {
	if len(argument) > 1 && argument[0] == '@' {
		return true
	}
	if argument == "-Xclang" || argument == "-Xpreprocessor" || argument == "-mllvm" ||
		argument == "-mmlir" || argument == "-mthread-model" || argument == "-fplugin" ||
		argument == "-B" || argument == "-specs" || argument == "-wrapper" ||
		argument == "-ivfsoverlay" || argument == "-vfsoverlay" ||
		argument == "--param" || argument == "--config" || argument == "-Xcuda-fatbinary" || argument == "-Xcuda-ptxas" ||
		argument == "--cuda-gpu-arch" || argument == "--offload-arch" {
		return true
	}
	if strings.HasPrefix(argument, "-Wp,") || strings.HasPrefix(argument, "-fplugin") ||
		strings.HasPrefix(argument, "-B") || strings.HasPrefix(argument, "-specs=") ||
		strings.HasPrefix(argument, "-wrapper=") ||
		strings.HasPrefix(argument, "-m") ||
		strings.HasPrefix(argument, "-f") ||
		strings.HasPrefix(argument, "--cuda-gpu-arch=") ||
		strings.HasPrefix(argument, "--cuda-") ||
		strings.HasPrefix(argument, "--offload-") {
		return true
	}
	return false
}

func isSafeMachineOption(argument string) bool {
	for _, prefix := range []string{
		"-march=", "-mcpu=", "-mtune=", "-mabi=", "-mfpu=", "-mfloat-abi=",
	} {
		if strings.HasPrefix(argument, prefix) && len(argument) > len(prefix) {
			return true
		}
	}

	switch argument {
	case "-m16", "-m32", "-m64", "-mx32", "-marm", "-mthumb", "-mthumb-interwork",
		"-mlittle-endian", "-mbig-endian", "-msoft-float", "-mhard-float",
		"-mgeneral-regs-only", "-mred-zone", "-mno-red-zone", "-mstackrealign",
		"-mlong-double-64", "-mlong-double-80", "-mlong-double-128",
		"-mmmx", "-mno-mmx", "-msse", "-mno-sse", "-msse2", "-mno-sse2",
		"-msse3", "-mno-sse3", "-mssse3", "-mno-ssse3", "-msse4.1", "-mno-sse4.1",
		"-msse4.2", "-mno-sse4.2", "-mavx", "-mno-avx", "-mavx2", "-mno-avx2",
		"-mavx512f", "-mno-avx512f", "-maes", "-mno-aes", "-mpclmul", "-mno-pclmul",
		"-mfma", "-mno-fma", "-mbmi", "-mno-bmi", "-mbmi2", "-mno-bmi2",
		"-mlzcnt", "-mno-lzcnt", "-mpopcnt", "-mno-popcnt", "-msha", "-mno-sha":
		return true
	default:
		return false
	}
}

func containsResponseFile(arguments []string) bool {
	for _, argument := range arguments {
		if len(argument) > 1 && argument[0] == '@' {
			return true
		}
	}
	return false
}

func (t *Tool) getPredefinedMacros(arguments []string, sourceFile, workingDir string) []string {
	if t.predefinedMacros == nil {
		t.predefinedMacros = make(map[string][]string)
	}
	compiler := arguments[0]
	probeCompiler := compiler
	if fullPath := compilerFullPath(compiler, workingDir); fullPath != "" {
		probeCompiler = fullPath
	}
	language := compilerLanguage(arguments, sourceFile)
	probeArgs, unsupportedReason := predefinedMacroProbeArgsChecked(arguments, language)
	cacheKey := workingDir + "\x00" + compilerCacheIdentity(probeCompiler) + "\x00" + strings.Join(probeArgs, "\x00")
	if name := unsafeCompilerEnvironment(os.Environ()); name != "" {
		cacheKey += "\x00environment\x00" + name
		if macros, ok := t.predefinedMacros[cacheKey]; ok {
			return macros
		}
		t.Logger.Errorf("failed to get predefined macros from %s: compiler-control environment variable %s is set", compiler, name)
		t.predefinedMacros[cacheKey] = nil
		return nil
	}
	if wrapper := compilerResolvedWrapper(probeCompiler); wrapper != "" {
		cacheKey += "\x00resolved-wrapper\x00" + wrapper
		if macros, ok := t.predefinedMacros[cacheKey]; ok {
			return macros
		}
		t.Logger.Errorf("failed to get predefined macros from %s: %s compiler selection cannot be determined safely", compiler, wrapper)
		t.predefinedMacros[cacheKey] = nil
		return nil
	}
	if containsResponseFile(arguments[1:]) {
		cacheKey += "\x00response\x00" + strings.Join(arguments[1:], "\x00")
		if macros, ok := t.predefinedMacros[cacheKey]; ok {
			return macros
		}
		t.Logger.Errorf("failed to get predefined macros from %s: response files are not supported", compiler)
		t.predefinedMacros[cacheKey] = nil
		return nil
	}
	if !supportedCompilerLanguage(language) {
		cacheKey += "\x00unsupported-language"
		if macros, ok := t.predefinedMacros[cacheKey]; ok {
			return macros
		}
		t.Logger.Errorf("failed to get predefined macros from %s: unsupported language %q", compiler, language)
		t.predefinedMacros[cacheKey] = nil
		return nil
	}
	if !isSinglePhaseCompilerLanguage(language) {
		cacheKey += "\x00multi-stage-language"
		if macros, ok := t.predefinedMacros[cacheKey]; ok {
			return macros
		}
		t.Logger.Errorf("failed to get predefined macros from %s: language %q uses multiple compilation stages", compiler, language)
		t.predefinedMacros[cacheKey] = nil
		return nil
	}
	if unsupportedReason != "" {
		cacheKey += "\x00unsupported-option\x00" + unsupportedReason + "\x00" + strings.Join(arguments[1:], "\x00")
		if macros, ok := t.predefinedMacros[cacheKey]; ok {
			return macros
		}
		t.Logger.Errorf("failed to get predefined macros from %s: %s", compiler, unsupportedReason)
		t.predefinedMacros[cacheKey] = nil
		return nil
	}
	if macros, ok := t.predefinedMacros[cacheKey]; ok {
		return macros
	}
	var cmd *exec.Cmd
	if t.compilerCommand != nil {
		cmd = t.compilerCommand(probeCompiler, probeArgs...)
		configureProcessCommandWithoutContext(cmd)
	} else {
		command := t.compilerCommandContext
		if command == nil {
			command = exec.CommandContext
		}
		cmd = command(t.operationContext(), probeCompiler, probeArgs...)
		configureProcessCommand(cmd, t.operationContext())
	}
	cmd.Dir = workingDir
	cmd.Stdin = strings.NewReader("\n")
	environment := cmd.Env
	if environment == nil {
		environment = os.Environ()
	}
	if name := unsafeCompilerEnvironment(environment); name != "" {
		t.Logger.Errorf("failed to get predefined macros from %s: compiler-control environment variable %s is set", compiler, name)
		t.predefinedMacros[cacheKey] = nil
		return nil
	}
	cmd.Env = probeEnvironment(environment, workingDir)
	output, err := outputProcessCommand(cmd, t.operationContext())
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && len(exitError.Stderr) > 0 {
			t.Logger.Errorf("failed to get predefined macros from %s: %v: %s", compiler, err, strings.TrimSpace(string(exitError.Stderr)))
		} else {
			t.Logger.Errorf("failed to get predefined macros from %s: %v", compiler, err)
		}
		t.predefinedMacros[cacheKey] = nil
		return nil
	}

	macros := parsePredefinedMacros(output)
	t.predefinedMacros[cacheKey] = macros
	return macros
}

func probeEnvironment(environment []string, workingDir ...string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if strings.EqualFold(name, "DEPENDENCIES_OUTPUT") || strings.EqualFold(name, "SUNPRO_DEPENDENCIES") ||
			len(workingDir) > 0 && strings.EqualFold(name, "PWD") {
			continue
		}
		filtered = append(filtered, value)
	}
	if len(workingDir) > 0 {
		filtered = append(filtered, "PWD="+workingDir[0])
	}
	return filtered
}

func unsafeCompilerEnvironment(environment []string) string {
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(name) {
		case "CCC_OVERRIDE_OPTIONS", "CCC_ADD_ARGS", "CCC_PRINT_OPTIONS", "CCC_PRINT_OPTIONS_FILE",
			"GCC_EXEC_PREFIX", "COMPILER_PATH", "GCC_COMPARE_DEBUG",
			"CLANG_CONFIG_FILE_SYSTEM_DIR", "CLANG_CONFIG_FILE_USER_DIR",
			"LD_PRELOAD", "LD_AUDIT", "DYLD_INSERT_LIBRARIES":
			return name
		}
	}
	return ""
}
