package internal

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

const (
	EncodingRaw     = "raw"
	EncodingGB18030 = "gb18030"
)

func strictSourceFile(filename string) error {
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	return nil
}

// slashPath converts a lexical path to the slash form used by existing
// compilation database compatibility code. It does not clean the path.
func slashPath(path string) string {
	newPath := strings.ReplaceAll(path, "\\", "/")

	if runtime.GOOS == "windows" {
		// Handle drive letter (e.g.: "/c/path -> c:/path")
		if len(newPath) > 2 && newPath[0] == '/' && newPath[2] == '/' {
			newPath = string(newPath[1]) + ":" + newPath[2:]
		} else if len(newPath) == 2 && newPath[0] == '/' {
			newPath = string(newPath[1]) + ":"
		}
	}

	return newPath
}

func hostPathToDatabasePath(path string) string {
	return slashPath(path)
}

func windowsPathToSlash(path string) string {
	return slashPath(path)
}

func trackedPathToSlash(path string) string {
	return slashPath(path)
}

// HasPathRootOrVolumePrefix accepts both host and foreign path syntax used by
// CLI input and output paths. It intentionally includes drive-relative forms.
func HasPathRootOrVolumePrefix(path string) bool {
	if strings.HasPrefix(path, "/") {
		return true
	}
	if strings.HasPrefix(path, "\\") || (len(path) > 1 && path[1] == ':') {
		return true
	}
	return false
}

func NormalizeEncoding(encoding string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(encoding))
	if value == "" {
		value = EncodingRaw
	}

	switch value {
	case EncodingRaw, EncodingGB18030:
		return value, nil
	default:
		return "", fmt.Errorf("unsupported encoding %q", encoding)
	}
}

func ShellJoinArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}

	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuoteArgument(arg))
	}

	return strings.Join(quoted, " ")
}

func shellQuoteArgument(argument string) string {
	if runtime.GOOS == "windows" {
		return windowsQuoteArgument(argument)
	}
	if argument != "" && strings.IndexFunc(argument, func(character rune) bool {
		return !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("_@%+=:,./-", character))
	}) < 0 {
		return argument
	}
	return "'" + strings.ReplaceAll(argument, "'", `'"'"'`) + "'"
}

func windowsQuoteArgument(argument string) string {
	if argument != "" && !strings.ContainsAny(argument, " \t\n\v\"") {
		return argument
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	backslashes := 0
	for _, character := range argument {
		if character == '\\' {
			backslashes++
			continue
		}
		if character == '"' {
			quoted.WriteString(strings.Repeat(`\`, backslashes*2+1))
			quoted.WriteRune(character)
			backslashes = 0
			continue
		}
		quoted.WriteString(strings.Repeat(`\`, backslashes))
		backslashes = 0
		quoted.WriteRune(character)
	}
	quoted.WriteString(strings.Repeat(`\`, backslashes*2))
	quoted.WriteByte('"')
	return quoted.String()
}
