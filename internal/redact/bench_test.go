package redact

import "testing"

// Representative durable-log lines: mostly ordinary output, which is the real
// steady state on the PTY reader goroutine.
var benchLines = []string{
	"2026/09/12 15:03:37 INFO state changed from=running to=stopping",
	"   ✓ internal/supervisor  1.492s",
	"cd /tmp/cairn-rv216 && go build ./... && go vet ./... && echo BUILD_VET_OK",
	"curl -s -H \"Authorization: token $GITEA_TOKEN\" https://gitea.stump.rocks/api/v1/repos/x/y",
	"git remote set-url origin https://user:hunter2@example.com/a/b.git",
	"        at foo.go:123 in packageName.functionName()",
}

func BenchmarkStringPerLine(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = String(benchLines[i%len(benchLines)])
	}
}

func BenchmarkStringOrdinaryLineOnly(b *testing.B) {
	ln := "   ✓ internal/supervisor  1.492s"
	for i := 0; i < b.N; i++ {
		_ = String(ln)
	}
}
