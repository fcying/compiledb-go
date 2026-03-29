package internal

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	EncodingRaw     = "raw"
	EncodingGB18030 = "gb18030"
)

func FileExist(filename string) bool {
	_, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return true
}

func GetBinFullPath(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

func ConvertPath(path string) string {
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

func IsAbsPath(path string) bool {
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
		if arg == "" {
			quoted = append(quoted, "''")
			continue
		}

		if strings.ContainsAny(arg, " \t\n\r'\"\\$`;&|()<>*?[]{}!") {
			quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", `'"'"'`)+"'")
			continue
		}

		quoted = append(quoted, arg)
	}

	return strings.Join(quoted, " ")
}

func TransferPrint(in io.Reader, out io.Writer, encoding string) {
	if encoding == EncodingRaw {
		if _, err := io.Copy(out, in); err != nil {
			fmt.Fprintln(out, "Error reading stream:", err)
		}
		return
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*100)
	decoder := simplifiedchinese.GB18030.NewDecoder()

	for scanner.Scan() {
		result, err := decoder.String(scanner.Text())
		if err != nil {
			fmt.Fprintln(out, "decode failed!", scanner.Text())
			continue
		}
		fmt.Fprintln(out, result)
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(out, "Error reading stream:", err)
	}
}
