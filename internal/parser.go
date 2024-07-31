package internal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Internal variables used to parse build log entries
// var cc_compile_regex, _ = regexp.Compile(`^.*-?g?cc-?[0-9.]*$|^.*-?clang-?[0-9.]*$`)
var cc_compile_regex = regexp.MustCompile(`^.*-?gcc-?.*(\.exe)?$|^.*-?clang-?.*(\.exe)?$`)
var cpp_compile_regex = regexp.MustCompile(`^.*-?[gc]\+\+-?[0-9.]*$|^.*-?clang\+\+-?[0-9.]*(\.exe)$`)

var file_regex = regexp.MustCompile(`^.*-c\s(.*\.(c|cpp|cc|cxx|s))\s|$`)
var compiler_wrappers []string = []string{"ccache", "icecc", "sccache"}

// Leverage `make --print-directory` option
var make_enter_dir = regexp.MustCompile(`^\s*make.*?: Entering directory .*$`)
var make_leave_dir = regexp.MustCompile(`^\s*make.*?: Leaving directory .*$`)

// We want to skip such lines from configure to avoid spurious MAKE expansion errors.
var checking_make = regexp.MustCompile(`^checking whether .* sets \$\(\w+\)\.\.\. (yes|no)$`)

var command_cnt = 0
var db []interface{}

func commandProcess(line string, workingDir string) ([]string, string) {
	arguments := []string{}
	filepath := ""
	log.Println("New command:", line)
	if cc_compile_regex.MatchString(line) ||
		cpp_compile_regex.MatchString(line) {
		arguments = strings.Fields(line)
		group := file_regex.FindStringSubmatch(line)
		if group != nil {
			filepath = group[1]
			log.Printf("Adding command %d: %s", command_cnt, line)
			command_cnt += 1
		}
	}
	return arguments, filepath
}

// func parse_build_log(build_log, proj_dir, exclude_files, command_style=False, add_predefined_macros=False,
//
//	use_full_path=False, extra_wrappers=[]){
func ParseBuildLog(buildLog string, outFileName string, command_style bool, noBuild bool) {
	log.Println("start parse log")

	file, err := os.OpenFile(buildLog, os.O_RDONLY, 0444)
	if err != nil {
		log.Fatalf("open %v failed!", buildLog)
	}
	defer file.Close()

	scnner := bufio.NewScanner(file)
	scnner.Buffer(make([]byte, 1024*1024), 1024*1024*100)

	// check workingDir
	workingDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("get workingDir failed! %v", err)
	}
	if buildLog != "stdin" {
		absPath, _ := filepath.Abs(buildLog)
		workingDir = filepath.Dir(absPath)
	}
	log.Printf("workingDir: %s", workingDir)

	for scnner.Scan() {
		line := scnner.Text()

		if line == "" {
			continue
		}

		// TODO Parse directory that make entering/leaving

		// Parse command
		arguments, filepath := commandProcess(line, workingDir)
		command := ""
		if filepath != "" {
			if command_style {
				command = strings.Join(arguments, " ")
				data := struct {
					Directory string `json:"directory"`
					Command   string `json:"command"`
					File      string `json:"file"`
				}{
					Directory: workingDir,
					Command:   command,
					File:      filepath,
				}
				db = append(db, data)
			} else {
				data := struct {
					Directory string   `json:"directory"`
					Arguments []string `json:"arguments"`
					File      string   `json:"file"`
				}{
					Directory: workingDir,
					Arguments: arguments,
					File:      filepath,
				}
				db = append(db, data)
			}

			// format
			jsonData, err := json.MarshalIndent(db, "", "  ")
			if err != nil {
				log.Fatalf("Error encoding JSON:%v", err)
			}

			// write file
			filename := outFileName
			outfile, err := os.Create(filename)
			if err != nil {
				log.Fatalf("create %v failed! err:%v", filename, err)
			}
			defer outfile.Close()

			_, err = outfile.Write(jsonData)
			if err != nil {
				log.Fatalf("write %v failed! err:%v", filename, err)
			}
		}
	}
}
