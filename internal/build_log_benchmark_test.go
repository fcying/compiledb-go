package internal

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sirupsen/logrus"
)

const benchmarkBuildLogChunkSize = 1 << 20

var benchmarkBuildLogSizes = []struct {
	name string
	size int64
}{
	{name: "10MiB", size: 10 << 20},
	{name: "100MiB", size: 100 << 20},
	{name: "500MiB", size: 500 << 20},
}

func BenchmarkBuildLogScan(b *testing.B) {
	chunk := buildLogBenchmarkChunk()
	for _, size := range benchmarkBuildLogSizes {
		b.Run(size.name, func(b *testing.B) {
			data := buildLogBenchmarkData(size.size, chunk)
			b.SetBytes(size.size)
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				lines := scanBuildLog(data)
				runtime.KeepAlive(lines)
			}
		})
	}
}

func BenchmarkBuildLogMergeLogicalLines(b *testing.B) {
	chunk := buildLogBenchmarkChunk()
	for _, size := range benchmarkBuildLogSizes {
		b.Run(size.name, func(b *testing.B) {
			data := buildLogBenchmarkData(size.size, chunk)
			lines := scanBuildLog(data)
			data = nil
			runtime.GC()

			b.SetBytes(size.size)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				logicalLines := mergeLogicalLines(lines)
				runtime.KeepAlive(logicalLines)
			}
		})
	}
}

func BenchmarkBuildLogPipeline(b *testing.B) {
	chunk := buildLogBenchmarkChunk()
	b.Run("DirectGenerate", func(b *testing.B) {
		for _, size := range benchmarkBuildLogSizes {
			b.Run(size.name, func(b *testing.B) {
				benchmarkDirectGenerate(b, size.size, chunk)
			})
		}
	})
	b.Run("DiscoveryOutput", func(b *testing.B) {
		for _, size := range benchmarkBuildLogSizes {
			b.Run(size.name, func(b *testing.B) {
				benchmarkDiscoveryOutput(b, size.size, chunk)
			})
		}
	})
}

func benchmarkDirectGenerate(b *testing.B, size int64, chunk []byte) {
	directory := b.TempDir()
	inputFile := filepath.Join(directory, "build.log")
	outputFile := filepath.Join(directory, "compile_commands.json")
	file, err := os.Create(inputFile)
	if err != nil {
		b.Fatal(err)
	}
	if err := writeBuildLogBenchmarkData(file, size, chunk); err != nil {
		file.Close()
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}

	tool := buildLogBenchmarkTool(inputFile, outputFile, directory)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tool.Generate()
		if tool.StatusCode != 0 {
			b.Fatalf("parser status: %d", tool.StatusCode)
		}
	}
}

func benchmarkDiscoveryOutput(b *testing.B, size int64, chunk []byte) {
	directory := b.TempDir()
	tool := buildLogBenchmarkTool("stdin", filepath.Join(directory, "compile_commands.json"), directory)
	tool.makeDirectoryMarkers = true
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		var stdout bytes.Buffer
		if err := writeBuildLogBenchmarkData(&stdout, size, chunk); err != nil {
			b.Fatal(err)
		}
		buildLog := scanBuildLog(stdout.Bytes())
		tool.parseBuildLog(buildLog)
		if tool.StatusCode != 0 {
			b.Fatalf("parser status: %d", tool.StatusCode)
		}
	}
}

func buildLogBenchmarkTool(inputFile, outputFile, buildDir string) *Tool {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return NewTool(Config{
		InputFile:    inputFile,
		OutputFile:   outputFile,
		BuildDir:     buildDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Overwrite:    true,
	}, logger)
}

func buildLogBenchmarkChunk() []byte {
	const (
		compileCommand = "cc -DBENCHMARK=1 \\\n-Igenerated \\\n-c benchmark.c\n"
		noiseLine      = "[benchmark] generated target remains unchanged; no compiler command was emitted\n"
	)

	chunk := make([]byte, 0, benchmarkBuildLogChunkSize)
	chunk = append(chunk, compileCommand...)
	for len(chunk)+len(noiseLine) <= cap(chunk) {
		chunk = append(chunk, noiseLine...)
	}
	for len(chunk) < cap(chunk)-1 {
		chunk = append(chunk, 'x')
	}
	return append(chunk, '\n')
}

func buildLogBenchmarkData(size int64, chunk []byte) []byte {
	data := make([]byte, size)
	for offset := 0; offset < len(data); {
		offset += copy(data[offset:], chunk)
	}
	return data
}

func writeBuildLogBenchmarkData(writer io.Writer, size int64, chunk []byte) error {
	for size > 0 {
		writeSize := int64(len(chunk))
		if writeSize > size {
			writeSize = size
		}
		written, err := writer.Write(chunk[:writeSize])
		if err != nil {
			return err
		}
		if written != int(writeSize) {
			return io.ErrShortWrite
		}
		size -= writeSize
	}
	return nil
}
