package main

import (
	"testing"

	"github.com/fcying/compiledb-go/internal"
	log "github.com/sirupsen/logrus"
)


func TestParser(t *testing.T) {
	log.Error("TestParser")
	internal.ParseBuildLog("../../tests/build.log", "compile_commands.json", true, false)
}
