package ingest

import "testing"

// benchLines is a stand-in for what an app actually writes: mostly plain lines, some JSON, the
// odd colored or tagged one. ParseLine runs once per ingested line for every app, so it is the
// hottest pure-compute path in the tree.
var benchLines = []string{
	`{"level":"error","message":"boom","request_id":"8a1b2c3d-FRA"}`,
	`{"level":"info","msg":"served","request_id":"9f2e1a4b-AMS"}`,
	`Started GET "/" for 10.0.0.1 at 2026-09-21 10:53:02 +0000`,
	`Completed 200 OK in 14ms (Views: 9.1ms | ActiveRecord: 2.3ms)`,
	"  app/models/user.rb:42:in `find_by_token' - retrying the lookup once more before giving up",
	"\x1b[36mProcessing\x1b[0m by HomeController#index as HTML",
	`[8a1b2c3d-FRA] Rendering layouts/application.html.erb`,
}

// jsonLine and plainLine isolate the two branches ParseLine takes.
const (
	jsonLine  = `{"level":"error","message":"boom","request_id":"8a1b2c3d-FRA"}`
	plainLine = `Completed 200 OK in 14ms (Views: 9.1ms | ActiveRecord: 2.3ms)`
)

func BenchmarkParseLineJSON(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkEntry = ParseLine("stdout", "web", jsonLine)
	}
}

// BenchmarkParseLinePlain is the worst case for detectLevel: no level keyword, so it makes every
// pass before falling back to "info".
func BenchmarkParseLinePlain(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkEntry = ParseLine("file", "production.log", plainLine)
	}
}

func BenchmarkParseLineMixed(b *testing.B) {
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		sinkEntry = ParseLine("stdout", "web", benchLines[i%len(benchLines)])
		i++
	}
}

// sinkEntry keeps the compiler from dropping the call.
var sinkEntry any
