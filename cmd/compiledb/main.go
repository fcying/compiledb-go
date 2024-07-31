package main

import (
	"os"

	"github.com/fcying/compiledb-go/internal"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

func init() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.WarnLevel)
	// log.SetLevel(log.InfoLevel)
}

func main() {
	cli.AppHelpTemplate = `{{.HelpName}} v{{.Version}}

USAGE: {{.Name}} {{if .VisibleFlags}}[options]{{end}}{{if .Commands}} command [command options]{{end}} {{if .ArgsUsage}}{{.ArgsUsage}}{{else}}[args]...
{{end}}
{{.Description}}
{{if .VisibleFlags}}
OPTIONS:
   {{range .VisibleFlags}}{{.}}
   {{end}}{{end}}{{if .Commands}}
COMMANDS:
{{range .Commands}}{{if not .HideHelp}}   {{join .Names ", "}}{{ "\t"}}{{.Usage}}{{ "\n" }}{{end}}{{end}}{{end}}
`
	app := &cli.App{
		EnableBashCompletion: true,
		Version:              "1.0.1",
		// Compiled:             time.Now()
		HideHelpCommand: true,
		HideVersion:     true,
		Name:            "compiledb",
		HelpName:        "compiledb-go",
		Description: "\tClang's Compilation Database generator for make-based build systems." +
			"\n\tWhen no subcommand is used it will parse build log/commands and generates" +
			"\n\tits corresponding Compilation datAbase.",
		Action: func(ctx *cli.Context) error {
			if ctx.Bool("v") {
				log.SetLevel(log.DebugLevel)
			}
			internal.ParseBuildLog(ctx.String("p"), ctx.String("o"), ctx.Bool("command-style"),
				ctx.Bool("no-build"),
			)
			log.Println("Done")
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:  "make",
				Usage: "Generates compilation database file for an arbitrary GNU Make...",
				Action: func(ctx *cli.Context) error {
					log.Error("TODO: make")
					return nil
				},
			},
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "parse",
				Aliases: []string{"p"},
				Usage:   "Build log file to parse compilation commands from `File`.",
				Value:   "stdin",
			},
			&cli.StringFlag{
				//   -o, --output FILENAME  Output file path (Default: compile_commands.json). If
				//                          -f/--overwrite is not specified, this file is updated
				//                          with the new contents. Use '-' to output to stdout
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "Output file to `File`.",
				Value:   "compile_commands.json",
			},
			// &cli.StringFlag{
			// 	Name:    "build-dir",
			// 	Aliases: []string{"d"},
			// 	Usage:   "`Path` to be used as initial build dir.",
			// },
			// &cli.StringFlag{
			// 	Name:    "exclude",
			// 	Aliases: []string{"e"},
			// 	Usage:   "Regular expressions to exclude files",
			// },
			&cli.BoolFlag{
				Name: "no-build", Aliases: []string{"n"},
				Usage:              "Only generates compilation db file",
				DisableDefaultText: true,
			},
			&cli.BoolFlag{
				Name:               "verbose",
				Aliases:            []string{"v"},
				Usage:              "Print verbose messages.",
				DisableDefaultText: true,
			},
			// &cli.BoolFlag{
			// 	Name:               "overwrite",
			// 	Aliases:            []string{"f"},
			// 	Usage:              "Overwrite compile_commands.json instead of just updating it.",
			// 	DisableDefaultText: true,
			// },
			// &cli.BoolFlag{
			// 	Name:               "no-strict",
			// 	Aliases:            []string{"S"},
			// 	Usage:              "Do not check if source files exist in the file system.",
			// 	DisableDefaultText: true,
			// },
			// &cli.BoolFlag{
			// 	Name:               "macros",
			// 	Aliases:            []string{"m"},
			// 	Usage:              "Add predefined compiler macros to the compilation database. Make sure that all of the used compilers are in your $PATH.",
			// 	DisableDefaultText: true,
			// },
			// &cli.BoolFlag{
			// 	Name:               "full-path",
			// 	Usage:              "Write full path to the compiler executable.",
			// 	DisableDefaultText: true,
			// },
			&cli.BoolFlag{
				Name:               "command-style",
				Aliases:            []string{"c"},
				Usage:              `Output compilation database with single "command" string rather than the default "arguments" list of strings.`,
				DisableDefaultText: true,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
