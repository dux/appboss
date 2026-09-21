package super

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// Echo mirrors process output to the terminal with a colored "app/proc | " prefix, foreman style.
// Lines from different processes never interleave because every write goes through one mutex.
type Echo struct {
	mu     sync.Mutex
	out    io.Writer
	next   int
	keys   map[string]string
	soloed bool
}

func NewEcho(out io.Writer) *Echo { return &Echo{out: out} }

// Solo drops the app segment from every key. Running `dboss start` inside an app folder serves
// exactly one app, so repeating its name on every line is noise: `job |` says as much as
// `sinatra/job |` when there is no other app to confuse it with.
func (e *Echo) Solo() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.soloed = true
}

// Name is the plain process label behind a key, which is also its printed width once the color
// escapes are stripped.
func (e *Echo) Name(app, proc string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.name(app, proc)
}

func (e *Echo) name(app, proc string) string {
	if e.soloed {
		return proc
	}
	return app + "/" + proc
}

// Key is the colored "app/proc |" prefix for one process, assigned on first ask and kept for
// the session. The startup banner prints a row under the same key the process will later log
// under, and a restart keeps its color, so a key always means the same process on screen.
func (e *Echo) Key(app, proc string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.key(app, proc)
}

func (e *Echo) key(app, proc string) string {
	name := e.name(app, proc)
	if prefix, ok := e.keys[name]; ok {
		return prefix
	}
	if e.keys == nil {
		e.keys = map[string]string{}
	}
	color := 31 + e.next%6
	e.next++
	prefix := fmt.Sprintf("\x1b[%dm%s |\x1b[0m ", color, name)
	e.keys[name] = prefix
	return prefix
}

// Print writes one already-keyed line through the same lock the process writers use, so a
// banner row can never land in the middle of a log line.
func (e *Echo) Print(line string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = io.WriteString(e.out, line+"\n")
}

func (e *Echo) writer(app, proc string) *echoWriter {
	e.mu.Lock()
	defer e.mu.Unlock()
	return &echoWriter{echo: e, prefix: e.key(app, proc)}
}

type echoWriter struct {
	echo    *Echo
	prefix  string
	partial []byte
}

func (w *echoWriter) Write(p []byte) (int, error) {
	w.echo.mu.Lock()
	defer w.echo.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		index := bytes.IndexByte(w.partial, '\n')
		if index < 0 {
			return len(p), nil
		}
		w.emit(w.partial[:index+1])
		w.partial = w.partial[index+1:]
	}
}

// flush prints a trailing line that never received its newline, typically the last output before exit.
func (w *echoWriter) flush() {
	w.echo.mu.Lock()
	defer w.echo.mu.Unlock()
	if len(w.partial) > 0 {
		w.emit(append(w.partial, '\n'))
		w.partial = nil
	}
}

func (w *echoWriter) emit(line []byte) {
	_, _ = io.WriteString(w.echo.out, w.prefix)
	_, _ = w.echo.out.Write(line)
}
