package main

import (
	"testing"

	"github.com/fcying/compiledb-go/internal"
	log "github.com/sirupsen/logrus"
)

func TestParser(t *testing.T) {
	log.Error("TestParser")
	internal.ParseConfig.InputFile = "../../tests/build.log"
	internal.ParseConfig.OutputFile = "compile_commands.json"
	internal.Generate()
}
