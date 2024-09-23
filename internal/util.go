package internal

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
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

func ConvertLinuxPath(path string) string {
	linuxPath := strings.ReplaceAll(path, "\\", "/")

	// Handle drive letter (e.g., "C:\path" -> "/c/path")
	// if len(linuxPath) > 1 && linuxPath[1] == ':' {
	// 	linuxPath = "/" + strings.ToLower(string(linuxPath[0])) + linuxPath[2:]
	// }

	return linuxPath
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

func TransferPrintScanner(in io.ReadCloser) {
	decoder := simplifiedchinese.GB18030.NewDecoder()
	scanner := bufio.NewScanner(in)

	for scanner.Scan() {
		result, err := decoder.String(scanner.Text())
		if err != nil {
			fmt.Println("decode failed!", scanner.Text())
			result = ""
		}
		fmt.Println(result)
	}

	if err := scanner.Err(); err != nil {
		fmt.Println("Error reading scanner:", err)
	}
}
